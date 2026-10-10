package engine

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/Mobius-Toolkit/Mobius/internal/github"
	"github.com/Mobius-Toolkit/Mobius/internal/runner"
	"github.com/Mobius-Toolkit/Mobius/internal/store"
)

//go:embed prompts/judge.md
var judgePrompt string

// The verdicts of the Judge.
const (
	fixVerdict      = "fix"
	questionVerdict = "question"
	followUpVerdict = "follow-up"
	rejectVerdict   = "reject"
)

// judgeItem is an open review thread or a conversation comment of a pull request with a new comment of a trusted
// user or bot.
type judgeItem struct {
	// id is the id of the first comment of the thread, or of the conversation comment.
	id int64
	// bot tells that the newest comment of the item comes from a trusted bot.
	bot  bool
	text string
	at   time.Time
	// comments are the ids of the new comments of trusted users in the item. A comment of a bot is not one of them.
	comments []int64
	// thread is the GraphQL node id of the review thread, or "" for a conversation comment.
	thread string
}

// itemVerdicts are the actions of the Judge for one item.
type itemVerdicts struct {
	Item    int64    `json:"item"`
	Actions []action `json:"actions"`
}

type action struct {
	Verdict string `json:"verdict"`
	Text    string `json:"text"`
}

// itemText is a text of the Judge for one item: the goal of a follow-up, or the reason of a reject.
type itemText struct {
	item int64
	text string
}

// judgeRoutes are the actions of the Judge by their route.
type judgeRoutes struct {
	// round holds the fix and question actions of each item.
	round     []itemVerdicts
	followUps []itemText
	rejects   []itemText
}

// judgeJob is the work of a Judge on the new items of the pull request of the task.
type judgeJob struct {
	task  store.Task
	title string
	body  string
	// from is the state of the task before the Judge.
	from        string
	pullRequest *gh.PullRequest
	items       []judgeItem
}

// quietItem is the newest item that the Judge saw for a task, with the time when it saw that item.
type quietItem struct {
	newest time.Time
	since  time.Time
}

// judge starts a Judge for the new items of the pull request of the task in checks, approval, ready_for_review,
// reviewed or needs_human, after review_quiet_period with no newer item. A task that waits for a human gets a Judge
// only for an item of a trusted user. With no new item, a reviewed task with no open thread waits for CI.
//
// The items come from what the poll read. A comment that a person edits or deletes does not show in the new comments of
// the poll, and GitHub does not document that a resolve of a review thread changes the update time of the pull
// request. Thus the pull request is read in full before the Judge starts. A change of the items at the read gives the
// next poll the decision.
//
// It gives true when the task has work for the Judge and does not wait for a human. With no new item, it gives
// otherWork when the task leaves the state reviewed. The drain holds each new Judge, also a Judge whose Worker starts
// after the start of the drain, and the next poll after a cancel starts it.
func (e *Engine) judge(ctx context.Context, repository github.Repository, task store.Task, pullRequest *gh.PullRequest, otherWork bool) (bool, error) {
	if e.draining() {
		return false, nil
	}
	items, err := e.newItems(ctx, repository, task, int64(pullRequest.GetNumber()))
	if err != nil {
		return false, err
	}
	if task.State == "needs_human" && !slices.ContainsFunc(items, func(item judgeItem) bool { return !item.bot }) {
		return false, nil
	}
	if len(items) == 0 {
		delete(e.quiet, task.ID)
		if task.State != "reviewed" {
			return false, nil
		}
		left, err := e.judgeReady(ctx, repository, task, pullRequest)
		return left && otherWork, err
	}
	work := task.State != "needs_human"
	newest := newestItem(items)
	seen, ok := e.quiet[task.ID]
	if !ok || !seen.newest.Equal(newest) {
		seen = quietItem{newest, time.Now()}
		e.quiet[task.ID] = seen
	}
	if time.Since(seen.since) < e.config.ReviewQuietPeriod {
		return work, nil
	}
	number := int64(pullRequest.GetNumber())
	if err := e.readPullRequestInFull(ctx, repository, number); err != nil {
		return false, err
	}
	fresh, err := e.newItems(ctx, repository, task, number)
	if err != nil {
		return false, err
	}
	if !slices.EqualFunc(items, fresh, func(a, b judgeItem) bool { return a.id == b.id && a.at.Equal(b.at) }) {
		return work, nil
	}
	delete(e.quiet, task.ID)
	issue, err := existingIssue(ctx, repository, task.Issue)
	if err != nil {
		return false, err
	}
	j := judgeJob{task: task, title: issue.GetTitle(), body: issue.GetBody(), from: task.State, pullRequest: pullRequest, items: fresh}
	e.startWorker(task.ID, func(ctx context.Context) { e.runJudge(ctx, j) })
	return work, nil
}

func newestItem(items []judgeItem) time.Time {
	var newest time.Time
	for _, item := range items {
		if item.at.After(newest) {
			newest = item.at
		}
	}
	return newest
}

// newItems gives the open review threads and the conversation comments of the pull request number of the task whose
// newest comment of a trusted user or bot is newer than the last items of the Judge. A comment of the Mobius App is
// no item. The first call for a pull request after the start of the server reads the pull request in full.
func (e *Engine) newItems(ctx context.Context, repository github.Repository, task store.Task, number int64) ([]judgeItem, error) {
	var judgedAt time.Time
	if task.JudgedAt.Valid {
		var err error
		if judgedAt, err = time.Parse(time.RFC3339Nano, task.JudgedAt.String); err != nil {
			return nil, err
		}
	}
	state := e.pull(repository, number)
	if !state.read {
		if err := e.readPullRequestInFull(ctx, repository, number); err != nil {
			return nil, err
		}
	}
	app := appLogin(repository.AppSlug)
	trusted := func(login string) bool { return e.TrustedAuthor(repository.AppSlug, login) }
	bot := func(login string) bool {
		return slices.ContainsFunc(e.config.TrustedBots, func(bot string) bool { return strings.EqualFold(bot, login) })
	}
	isNew := func(login string, at time.Time) bool {
		return trusted(login) && !strings.EqualFold(login, app) && at.After(judgedAt)
	}
	var items []judgeItem
	for _, open := range state.Threads {
		if !openThread(open, trusted, app) {
			continue
		}
		var newest *github.ThreadComment
		var comments []int64
		for i, comment := range open.Comments {
			if trusted(comment.Author) && (newest == nil || !comment.CreatedAt.Before(newest.CreatedAt)) {
				newest = &open.Comments[i]
			}
			if isNew(comment.Author, comment.CreatedAt) && !bot(comment.Author) {
				comments = append(comments, comment.ID)
			}
		}
		if newest != nil && isNew(newest.Author, newest.CreatedAt) {
			items = append(items, judgeItem{id: open.Comment, bot: bot(newest.Author), text: threadText(open, trusted), at: newest.CreatedAt, comments: comments, thread: open.ID})
		}
	}
	for _, id := range slices.Sorted(maps.Keys(state.conversation)) {
		comment := state.conversation[id]
		login, at := comment.GetUser().GetLogin(), comment.GetCreatedAt().Time
		if isNew(login, at) {
			var comments []int64
			if !bot(login) && e.commentIsEvent(repository.AppSlug, comment) {
				comments = []int64{id}
			}
			items = append(items, judgeItem{id: id, bot: bot(login), text: fmt.Sprintf("\nComment %d:\n%s", id, entry(login, at, "", comment.GetBody())), at: at, comments: comments})
		}
	}
	return items, nil
}

// threadText gives the text of the review thread: the comments of trusted authors.
func threadText(thread github.ReviewThread, trusted func(string) bool) string {
	line := ""
	if thread.Line != 0 {
		line = fmt.Sprintf(" line %d", thread.Line)
	}
	text := fmt.Sprintf("\nThread %d, %s%s:\n", thread.Comment, thread.Path, line)
	for _, comment := range thread.Comments {
		if trusted(comment.Author) {
			text += entry(comment.Author, comment.CreatedAt, "", comment.Body)
		}
	}
	return text
}

// judgeReady moves the reviewed task to checks when its pull request has no open thread, with a Mobius check run on
// the head. It gives true when the task left the state reviewed.
func (e *Engine) judgeReady(ctx context.Context, repository github.Repository, task store.Task, pullRequest *gh.PullRequest) (bool, error) {
	trusted := func(login string) bool { return e.TrustedAuthor(repository.AppSlug, login) }
	number := int64(pullRequest.GetNumber())
	if slices.ContainsFunc(e.pull(repository, number).Threads, func(thread github.ReviewThread) bool {
		return openThread(thread, trusted, appLogin(repository.AppSlug))
	}) {
		return false, nil
	}
	if err := setCheckRun(ctx, repository, pullRequest.GetHead().GetSHA(), "in_progress", "", "", ""); err != nil {
		return false, err
	}
	_, err := e.setTaskState(ctx, store.SetTaskStateParams{State: "checks", ID: task.ID, FromState: "reviewed"})
	return true, err
}

// runJudge moves the task to working and runs the Judge of the job until the task stops. The Worker of the task exists
// before the state change, so each stop after the state change stops the Judge. A failed Judge still marks its items
// as judged, so the same items start no new Judge, and it hands the task to a human.
func (e *Engine) runJudge(ctx context.Context, j judgeJob) {
	if !e.tryTrack() {
		return
	}
	moved, err := e.queries.StartTaskWorker(ctx, store.StartTaskWorkerParams{
		Worker:      sql.NullString{String: JudgeRole, Valid: true},
		WorkerInput: sql.NullString{String: j.from, Valid: true},
		ID:          j.task.ID,
		FromState:   j.from,
	})
	if err != nil || moved == 0 {
		e.untrack()
		if err != nil {
			log.Printf("Judge of %s#%d: %v", j.task.Repository, j.task.Issue, err)
		}
		return
	}
	e.publishReadyForReview(j.from, "working")
	err = e.judgeSession(ctx, j)
	if err == nil {
		return
	}
	log.Printf("Judge of %s#%d: %v", j.task.Repository, j.task.Issue, err)
	ended := context.WithoutCancel(ctx)
	if err := e.markJudged(ended, j); err != nil {
		log.Printf("Judge of %s#%d: %v", j.task.Repository, j.task.Issue, err)
	}
	reason := fmt.Sprintf("the Judge failed. Mobius added mobius:needs-human. The error ends with these lines:\n\n```\n%s\n```", tail(err.Error(), errorTail))
	if err := e.handOver(ended, j.task, j.title, reason); err != nil {
		log.Printf("stop of %s#%d: %v", j.task.Repository, j.task.Issue, err)
	}
}

// markJudged makes the newest item of the job the last item of the Judge.
func (e *Engine) markJudged(ctx context.Context, j judgeJob) error {
	return e.queries.SetJudgedAt(ctx, store.SetJudgedAtParams{
		JudgedAt: sql.NullString{String: newestItem(j.items).UTC().Format(time.RFC3339Nano), Valid: true},
		ID:       j.task.ID,
	})
}

// judgeSession runs one Judge session of the job in a worktree detached at the head of the pull request, and then
// routes the actions of the Judge. The drain counts the session from a tryTrack of the caller. The end of ctx stops
// the session.
func (e *Engine) judgeSession(ctx context.Context, j judgeJob) error {
	parent, err := e.judgeParent(ctx, j.task)
	if err != nil {
		e.untrack()
		return err
	}
	a, err := e.addAgent(ctx, Spec{
		Role:         JudgeRole,
		Organization: owner(j.task.Repository),
		Repository:   j.task.Repository,
		Workstream:   j.task.Workstream,
		Issue:        sql.NullInt64{Int64: j.task.Issue, Valid: true},
		Parent:       parent,
		Tracked:      true,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	ended := context.WithoutCancel(ctx)
	a.spec.Dir = runner.JudgeDir(e.config.DataDir, j.task.Repository, a.id)
	a.items = j.items
	err = e.judgeTurn(ctx, a, j)
	a.closeHarness()
	e.gitMu.Lock()
	if _, statErr := os.Stat(a.spec.Dir); !errors.Is(statErr, fs.ErrNotExist) {
		err = errors.Join(err, runner.RemoveWorktree(ended, e.config.DataDir, j.task.Repository, a.spec.Dir))
	}
	e.gitMu.Unlock()
	switch {
	case ctx.Err() != nil:
		return a.End(ended, "stopped")
	case errors.Is(err, errHung):
		return errors.Join(err, e.endHungTask(ended, a, j.task, j.title))
	case err != nil:
		return a.Fail(ended, err)
	}
	if err := a.End(ended, "done"); err != nil {
		return err
	}
	a.mu.Lock()
	verdicts := a.verdicts
	a.mu.Unlock()
	return e.route(ended, j, verdicts)
}

// judgeParent gives the parent of a new Judge of the task: the parent of the Judge that a restart of the server ended,
// or the newest session of the issue.
func (e *Engine) judgeParent(ctx context.Context, task store.Task) (sql.NullInt64, error) {
	last, err := e.newestSession(ctx, task)
	if err != nil || !last.Valid {
		return last, err
	}
	session, err := e.queries.GetSession(ctx, last.Int64)
	if err != nil {
		return sql.NullInt64{}, err
	}
	if session.Role == JudgeRole && session.EndReason.String == "restart" {
		return e.restartParent(ctx, task, JudgeRole)
	}
	return last, nil
}

// judgeTurn makes the worktree of the Judge a at the head of the pull request, and runs the turn of the Judge on the
// items of the job.
func (e *Engine) judgeTurn(ctx context.Context, a *Agent, j judgeJob) error {
	repository, err := e.repository(j.task.Repository)
	if err != nil {
		return err
	}
	token, err := repository.Token(ctx)
	if err != nil {
		return err
	}
	e.gitMu.Lock()
	err = runner.Fetch(ctx, e.config.DataDir, repository.FullName, repository.CloneURL, token)
	if err == nil {
		err = runner.AddDetachedWorktree(ctx, e.config.DataDir, repository.FullName, a.spec.Dir, j.pullRequest.GetHead().GetSHA())
	}
	e.gitMu.Unlock()
	if err != nil {
		return err
	}
	sections, err := e.repositorySections(ctx, repository, JudgeRole)
	if err != nil {
		return err
	}
	brief, err := brief(ctx, repository, j.task.Workstream)
	if err != nil {
		return err
	}
	var items strings.Builder
	for _, item := range j.items {
		items.WriteString(item.text)
	}
	prompt := fmt.Sprintf("%s\n%s# Brief\n\n%s\n\n# Issue\n\n#%d %s\n\n%s\n\n# Items\n%s", judgePrompt, sections, brief, j.task.Issue, j.title, j.body, items.String())
	if err := a.waitForPause(ctx); err != nil {
		return err
	}
	if err := a.open(ctx); err != nil {
		return err
	}
	for _, item := range j.items {
		for _, id := range item.comments {
			launched(ctx, repository, item.thread != "", id)
		}
	}
	return a.Prompt(ctx, prompt, nil)
}

// route acts on the verdicts of the Judge: a reject replies with its reason, a follow-up goes to the Lead, and the fix
// and question actions go to a fix round. A round with no fix action does not count toward max_fix_rounds. With no
// round, a needs_human task continues as continueTask says, and another task goes back to its state before the Judge.
func (e *Engine) route(ctx context.Context, j judgeJob, verdicts []itemVerdicts) error {
	repository, err := e.repository(j.task.Repository)
	if err != nil {
		return err
	}
	number := int64(j.pullRequest.GetNumber())
	routes := routesOf(j.items, verdicts)
	if err := e.markJudged(ctx, j); err != nil {
		return err
	}
	for _, reject := range routes.rejects {
		if _, err := reply(ctx, repository, number, reject.item, reject.text); err != nil {
			return err
		}
	}
	for _, followUp := range routes.followUps {
		var text string
		if index := slices.IndexFunc(j.items, func(item judgeItem) bool { return item.id == followUp.item }); index >= 0 {
			text = j.items[index].text
		}
		event := fmt.Sprintf("%s follow-up on pull request #%d of #%d \"%s\", item %d:\n\n> %s\n%s",
			time.Now().UTC().Format(timeFormat), number, j.task.Issue, j.title, followUp.item, followUp.text, text)
		if err := e.addLeadEvent(ctx, j.task.Repository, j.task.Workstream, sql.NullInt64{Int64: j.task.Issue, Valid: true}, "follow_up", event); err != nil {
			return err
		}
	}
	if len(routes.round) == 0 {
		if j.from == "needs_human" {
			_, err := e.continueTask(ctx, repository, j.title, j.task, j.pullRequest, "working")
			return err
		}
		_, err := e.setTaskState(ctx, store.SetTaskStateParams{State: j.from, ID: j.task.ID, FromState: "working"})
		return err
	}
	if j.from == "needs_human" {
		if err := e.unstop(ctx, repository, j.task); err != nil {
			return err
		}
	}
	counts := slices.ContainsFunc(routes.round, func(item itemVerdicts) bool {
		return slices.ContainsFunc(item.Actions, func(a action) bool { return a.Verdict == fixVerdict })
	})
	parent, err := e.newestSession(ctx, j.task)
	if err != nil {
		return err
	}
	return e.fixRound(ctx, repository, round{task: j.task, title: j.title, pullRequest: j.pullRequest, counts: counts, items: roundText(j.items, routes.round), parent: parent})
}

// roundText gives the prompt text of the items of the round with their actions.
func roundText(items []judgeItem, round []itemVerdicts) string {
	var text strings.Builder
	for _, verdicts := range round {
		if index := slices.IndexFunc(items, func(item judgeItem) bool { return item.id == verdicts.Item }); index >= 0 {
			text.WriteString(items[index].text)
		}
		for _, a := range verdicts.Actions {
			if a.Text == "" {
				fmt.Fprintf(&text, "\nAction: %s\n", a.Verdict)
			} else {
				fmt.Fprintf(&text, "\nAction: %s: %s\n", a.Verdict, a.Text)
			}
		}
	}
	return text.String()
}

// routesOf gives the routes of the verdicts of the items. With no valid call of submit_verdicts, each item goes to the
// fix round with the action fix.
func routesOf(items []judgeItem, verdicts []itemVerdicts) judgeRoutes {
	var routes judgeRoutes
	if verdicts == nil {
		for _, item := range items {
			routes.round = append(routes.round, itemVerdicts{item.id, []action{{Verdict: fixVerdict}}})
		}
		return routes
	}
	for _, item := range verdicts {
		var round []action
		for _, a := range item.Actions {
			switch a.Verdict {
			case fixVerdict, questionVerdict:
				round = append(round, a)
			case followUpVerdict:
				routes.followUps = append(routes.followUps, itemText{item.Item, a.Text})
			case rejectVerdict:
				routes.rejects = append(routes.rejects, itemText{item.Item, a.Text})
			}
		}
		if len(round) > 0 {
			routes.round = append(routes.round, itemVerdicts{item.Item, round})
		}
	}
	return routes
}

// validateVerdicts checks that the verdicts give each item of the batch one time, with one action or more. Items of
// trusted users take fix, question and follow-up, and items of trusted bots take fix and reject.
func validateVerdicts(items []judgeItem, verdicts []itemVerdicts) error {
	for _, item := range items {
		if count := len(slices.DeleteFunc(slices.Clone(verdicts), func(v itemVerdicts) bool { return v.Item != item.id })); count != 1 {
			return refuse("Give item %d one time.", item.id)
		}
	}
	for _, verdict := range verdicts {
		index := slices.IndexFunc(items, func(item judgeItem) bool { return item.id == verdict.Item })
		if index < 0 {
			return refuse("%d is not an item of this batch.", verdict.Item)
		}
		bot := items[index].bot
		if len(verdict.Actions) == 0 {
			return refuse("Give item %d one action or more.", verdict.Item)
		}
		for _, a := range verdict.Actions {
			if empty(a.Text) {
				return refuse("Each action of item %d needs a text.", verdict.Item)
			}
			switch a.Verdict {
			case fixVerdict:
			case questionVerdict, followUpVerdict:
				if bot {
					return refuse("Item %d is from a trusted bot, so its actions are only fix and reject.", verdict.Item)
				}
			case rejectVerdict:
				if !bot {
					return refuse("Item %d is from a trusted user, so its actions are only fix, question, and follow-up.", verdict.Item)
				}
			default:
				return refuse("%q is not a verdict. A verdict is fix, question, follow-up, or reject.", a.Verdict)
			}
		}
	}
	return nil
}

// submitVerdicts keeps the verdicts of the Judge of c for the end of its turn. A later valid call replaces an earlier
// one.
func (e *Engine) submitVerdicts(_ context.Context, c caller, _ github.Repository, input verdictsInput) (string, error) {
	c.agent.mu.Lock()
	defer c.agent.mu.Unlock()
	if err := validateVerdicts(c.agent.items, input.Items); err != nil {
		return "", err
	}
	c.agent.verdicts = input.Items
	return "Mobius routes the actions when your turn ends.", nil
}
