package engine

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/Mobius-Toolkit/Mobius/internal/github"
	"github.com/Mobius-Toolkit/Mobius/internal/runner"
	"github.com/Mobius-Toolkit/Mobius/internal/store"
)

//go:embed prompts/implementer.md
var implementerPrompt string

const (
	// checkRunName is the name of the check run of Mobius on the head of a pull request.
	checkRunName = "Mobius"
	// conflictRoundWorker is the Worker of a task in a conflict round, in the tasks table.
	conflictRoundWorker = "conflict_round"
	// checkRoundWorker is the Worker of a task in a fix round for a failed check run, in the tasks table.
	checkRoundWorker = "check_round"
	// logTail is the number of characters of the end of the output of a check in a prompt and in a check run.
	// GitHub allows at most 65535 characters in the summary of a check run.
	logTail = 60_000
	// diskFull is in the output of a check that failed on a full disk.
	diskFull = "No space left on device"
)

// job is the work of an Implementer: a new ticket, a fix round or a conflict round.
type job struct {
	task  store.Task
	title string
	// branch is the branch of the task, or "" before the first worktree of the task.
	branch string
	// pullRequest is the pull request that the job works on, or nil for a new ticket. A start after a cannot_do in a
	// fix round or a conflict round works on the pull request of that round.
	pullRequest   *gh.PullRequest
	conflictRound bool
	prompt        string
	// parent is the session of the agent that started the work, or the newest session of the issue when Mobius started it.
	parent sql.NullInt64
}

// outcome is the end of an Implementer session with no error. It is the end reason of the session.
type outcome string

const (
	done          outcome = "done"
	cannotDo      outcome = "cannot_do"
	checkFailed   outcome = "check_failed"
	notMerged     outcome = "not_merged"
	pushRejected  outcome = "push_rejected"
	mergeConflict outcome = "merge_conflict"
)

// result is the outcome of an Implementer session with its text: the reason of cannot_do, the end of the output of
// the failed check, the error of the rejected push, or the error of the merge with conflicts.
type result struct {
	outcome outcome
	text    string
	pushed  pushed
}

// pushed is the head that a done Implementer pushed, with its pull request.
type pushed struct {
	pullRequest *gh.PullRequest
	head        string
}

// round is a fix round on the pull request of the task with the open items.
type round struct {
	task        store.Task
	title       string
	pullRequest *gh.PullRequest
	// counts tells that the round counts toward max_fix_rounds. A round with no fix action does not count.
	counts bool
	// items is the prompt text of the open items with their actions.
	items string
	// parent is the session of the agent whose result started the round.
	parent sql.NullInt64
	// failedCheck tells that the round is for a failed check run. Only such a round goes before a new task in the queue.
	failedCheck bool
}

// startImplementer starts an Implementer for the dispatched task of the issue number in the Workstream. The Implementer
// gets the Brief, the issue and the instructions.
func (e *Engine) startImplementer(ctx context.Context, repository github.Repository, workstream, number int64, instructions string, parent sql.NullInt64) (string, error) {
	task, err := e.workstreamTask(ctx, repository, workstream, number)
	if err != nil {
		return "", err
	}
	defer e.holdWorker(task.ID)()
	brief, err := brief(ctx, repository, workstream)
	if err != nil {
		return "", err
	}
	sections, err := e.repositorySections(ctx, repository, ImplementerRole)
	if err != nil {
		return "", err
	}
	issue, err := existingIssue(ctx, repository, number)
	if err != nil {
		return "", err
	}
	text, err := e.readIssue(ctx, repository, number)
	if err != nil {
		return "", err
	}
	var pullRequest *gh.PullRequest
	if task.PullRequest.Valid {
		if pullRequest, err = repository.PullRequest(ctx, task.PullRequest.Int64); err != nil {
			return "", err
		}
	}
	queued, err := e.queries.QueueTask(ctx, store.QueueTaskParams{QueuedAt: sql.NullString{String: now(), Valid: true}, ID: task.ID, FromState: "dispatched"})
	if err != nil {
		return "", err
	}
	if queued == 0 {
		return "", refuse("The task of #%d is %s, not dispatched.", number, task.State)
	}
	if err := repository.RemoveLabel(ctx, number, questionLabel); err != nil {
		log.Printf("remove %s from %s#%d: %v", questionLabel, repository.FullName, number, err)
	}
	j := job{
		task:        task,
		title:       issue.GetTitle(),
		branch:      task.Branch.String,
		pullRequest: pullRequest,
		prompt:      fmt.Sprintf("%s\n%s# Brief\n\n%s\n\n# Issue\n\n%s\n# Lead instructions\n\n%s", implementerPrompt, sections, brief, text, instructions),
		parent:      parent,
	}
	if err := e.setWorker(ctx, task.ID, ImplementerRole, j.prompt); err != nil {
		return "", err
	}
	e.runImplementer(j)
	return fmt.Sprintf("Started an Implementer for #%d.", number), nil
}

func (e *Engine) startImplementerTool(ctx context.Context, c caller, repository github.Repository, input implementerInput) (string, error) {
	if input.N < 1 {
		return "", refuse("n must be 1 or more.")
	}
	if empty(input.Instructions) {
		return "", refuse("instructions must not be empty.")
	}
	return e.startImplementer(ctx, repository, c.workstream, input.N, input.Instructions, sql.NullInt64{Int64: c.session, Valid: true})
}

func (e *Engine) startFixRound(ctx context.Context, c caller, repository github.Repository, input findingsInput) (string, error) {
	if input.N < 1 {
		return "", refuse("n must be 1 or more.")
	}
	if empty(input.Findings) {
		return "", refuse("findings must not be empty.")
	}
	task, err := e.workstreamTask(ctx, repository, c.workstream, input.N)
	if err != nil {
		return "", err
	}
	defer e.holdWorker(task.ID)()
	if !task.PullRequest.Valid || !task.Branch.Valid {
		return "", refuse("The task of #%d has no pull request.", input.N)
	}
	pullRequest, err := repository.PullRequest(ctx, task.PullRequest.Int64)
	if err != nil {
		return "", err
	}
	issue, err := existingIssue(ctx, repository, input.N)
	if err != nil {
		return "", err
	}
	if task.State != "checks" && task.State != "approval" && task.State != "ready_for_review" {
		return "", refuse("The task of #%d is %s, not checks, approval or ready_for_review.", input.N, task.State)
	}
	moved, err := e.setTaskState(ctx, store.SetTaskStateParams{State: "working", ID: task.ID, FromState: task.State})
	if err != nil {
		return "", err
	}
	if moved == 0 {
		return "", refuse("The task of #%d is not %s any more.", input.N, task.State)
	}
	r := round{task: task, title: issue.GetTitle(), pullRequest: pullRequest, counts: true, items: "\nFindings of the Lead:\n" + input.Findings + "\n", parent: sql.NullInt64{Int64: c.session, Valid: true}}
	if err := e.fixRound(ctx, repository, r); err != nil {
		_, stateErr := e.setTaskState(ctx, store.SetTaskStateParams{State: task.State, ID: task.ID, FromState: "working"})
		return "", errors.Join(err, stateErr)
	}
	return fmt.Sprintf("Sent the findings to a fix round of #%d. At max_fix_rounds, Mobius stops the task instead.", input.N), nil
}

// approvePullRequest moves the task from approval to ready_for_review, sets the Mobius check of the head to success,
// makes a draft pull request ready for review, replaces mobius:working with mobius:review, and adds the Inbox item for
// the Owner.
func (e *Engine) approvePullRequest(ctx context.Context, c caller, repository github.Repository, input numberInput) (string, error) {
	if input.N < 1 {
		return "", refuse("n must be 1 or more.")
	}
	task, err := e.workstreamTask(ctx, repository, c.workstream, input.N)
	if err != nil {
		return "", err
	}
	if !task.PullRequest.Valid {
		return "", refuse("The task of #%d has no pull request.", input.N)
	}
	if task.State == "checks" {
		return "", refuse("The task of #%d still waits for CI, so it is not ready for approval.", input.N)
	}
	if task.State != "approval" {
		return "", refuse("The task of #%d is %s, not approval.", input.N, task.State)
	}
	pullRequest, err := repository.PullRequest(ctx, task.PullRequest.Int64)
	if err != nil {
		return "", err
	}
	checkRun, err := openCheckRun(ctx, repository, pullRequest.GetHead().GetSHA())
	if err != nil {
		return "", err
	}
	if checkRun == 0 {
		if _, err := e.setTaskState(ctx, store.SetTaskStateParams{State: "checks", ID: task.ID, FromState: "approval"}); err != nil {
			return "", err
		}
		return "", refuse("The head of the pull request of #%d changed after the CI passed. The task waits for the CI of the new head.", input.N)
	}
	issue, err := existingIssue(ctx, repository, input.N)
	if err != nil {
		return "", err
	}
	moved, err := e.setTaskState(ctx, store.SetTaskStateParams{State: "ready_for_review", ID: task.ID, FromState: "approval"})
	if err != nil {
		return "", err
	}
	if moved == 0 {
		return "", refuse("The task of #%d is not approval any more.", input.N)
	}
	if err := e.approve(ctx, repository, task, issue.GetTitle(), pullRequest, checkRun); err != nil {
		_, stateErr := e.setTaskState(ctx, store.SetTaskStateParams{State: "approval", ID: task.ID, FromState: "ready_for_review"})
		return "", errors.Join(err, stateErr)
	}
	return fmt.Sprintf("Approved pull request #%d of #%d. The Owner got it for review.", pullRequest.GetNumber(), input.N), nil
}

// openCheckRun returns the id of the Mobius check run of the head that is not complete, or 0 when it has none. A head
// that passed the CI check of Mobius has one.
func openCheckRun(ctx context.Context, repository github.Repository, head string) (int64, error) {
	runs, err := repository.CheckRuns(ctx, head)
	if err != nil {
		return 0, err
	}
	for _, run := range runs {
		if run.GetName() == checkRunName && run.GetStatus() != "completed" {
			return run.GetID(), nil
		}
	}
	return 0, nil
}

// setCheckRun sets the Mobius check run of the head. It changes the check run that is not complete, or adds one when
// the head has none. A head has a maximum of one Mobius check run that is not complete.
func setCheckRun(ctx context.Context, repository github.Repository, head, status, conclusion, title, summary string) error {
	checkRun, err := openCheckRun(ctx, repository, head)
	if err != nil {
		return err
	}
	if checkRun == 0 {
		return repository.CreateCheckRun(ctx, checkRunName, head, status, conclusion, title, summary)
	}
	return repository.UpdateCheckRun(ctx, checkRun, checkRunName, status, conclusion, title, summary)
}

// replaceCheckRun completes the Mobius check run of the old head that is not complete, because a new head replaced the
// old head.
func replaceCheckRun(ctx context.Context, repository github.Repository, head string) error {
	checkRun, err := openCheckRun(ctx, repository, head)
	if err != nil || checkRun == 0 {
		return err
	}
	return repository.UpdateCheckRun(ctx, checkRun, checkRunName, "completed", "neutral", "Replaced by a new head", "A new head of the pull request replaced this commit.")
}

func (e *Engine) approve(ctx context.Context, repository github.Repository, task store.Task, title string, pullRequest *gh.PullRequest, checkRun int64) error {
	if err := repository.CompleteCheckRun(ctx, checkRun, checkRunName, "success"); err != nil {
		return err
	}
	if pullRequest.GetDraft() {
		if err := repository.MarkReadyForReview(ctx, pullRequest.GetNodeID()); err != nil {
			return err
		}
	}
	if err := repository.AddLabel(ctx, task.Issue, reviewLabel); err != nil {
		return err
	}
	if err := repository.RemoveLabel(ctx, task.Issue, workingLabel); err != nil {
		return err
	}
	err := e.addInboxItem(ctx, store.AddInboxItemParams{
		Kind:         readyForReviewKind,
		Organization: repository.Owner(),
		Repository:   task.Repository,
		Workstream:   task.Workstream,
		Issue:        task.Issue,
		Text:         fmt.Sprintf("Pull request #%d of #%d \"%s\" is ready for review.", pullRequest.GetNumber(), task.Issue, title),
		Link:         pullRequest.GetHTMLURL(),
	})
	return err
}

// cannotDo ends the turn of the Implementer with the reason for the Lead.
func (e *Engine) cannotDo(ctx context.Context, c caller, repository github.Repository, input reasonInput) (string, error) {
	if empty(input.Reason) {
		return "", refuse("reason must not be empty.")
	}
	task, err := e.workstreamTask(ctx, repository, c.workstream, c.agent.spec.Issue.Int64)
	if err != nil {
		return "", err
	}
	unpushed, err := runner.HasUnpushedWork(ctx, e.config.DataDir, c.agent.spec.Dir, task.Branch.String, repository.DefaultBranch)
	if err != nil {
		return "", err
	}
	if unpushed {
		return "", refuse("Your worktree has work that Mobius did not push. Commit your work and end the turn normally. Mobius then runs the check and pushes the work. Do not call `cannot_do` to wait for a command or for the check.")
	}
	c.agent.mu.Lock()
	c.agent.cannotDo = input.Reason
	c.agent.mu.Unlock()
	return "Mobius ends this turn.", c.agent.cancel(ctx)
}

// holdReply keeps the reply of the Implementer of a fix round until the push of its commits.
func (e *Engine) holdReply(ctx context.Context, c caller, repository github.Repository, input threadInput) (string, error) {
	if empty(input.Text) {
		return "", refuse("text must not be empty.")
	}
	pullRequest := c.agent.spec.PullRequest
	if pullRequest == 0 {
		return "", refuse("Only an Implementer of a fix round can reply in a thread.")
	}
	target, found, err := findTarget(ctx, repository, pullRequest, input.Thread)
	if err != nil {
		return "", err
	}
	if !found {
		return "", refuse("%d is not a review thread or a comment of pull request #%d.", input.Thread, pullRequest)
	}
	c.agent.mu.Lock()
	c.agent.replies = append(c.agent.replies, heldReply{target, input.Text})
	c.agent.mu.Unlock()
	return "Mobius posts the reply after it pushes your commits.", nil
}

// fixRound starts a fix round of the Implementer on the pull request of the working task. The pull request is a draft
// during the round. At max_fix_rounds, a round that counts hands the task to a human instead. A task that is not
// working, for example after a decline, gets no round. The steps after the queue step run in the Worker, so an error
// there starts the Implementer of the round again.
func (e *Engine) fixRound(ctx context.Context, repository github.Repository, r round) error {
	if err := makeDraft(ctx, repository, r.pullRequest); err != nil {
		return err
	}
	if r.counts {
		added, err := e.queries.AddFixRound(ctx, store.AddFixRoundParams{ID: r.task.ID, Max: int64(e.config.MaxFixRounds)})
		if err != nil {
			return err
		}
		if added == 0 {
			return e.stopAtLimit(ctx, repository, r, "fix")
		}
	}
	brief, err := brief(ctx, repository, r.task.Workstream)
	if err != nil {
		return err
	}
	sections, err := e.repositorySections(ctx, repository, ImplementerRole)
	if err != nil {
		return err
	}
	issue, err := existingIssue(ctx, repository, r.task.Issue)
	if err != nil {
		return err
	}
	comments, err := e.readPullRequestComments(ctx, repository, int64(r.pullRequest.GetNumber()))
	if err != nil {
		return err
	}
	queued, err := e.queries.QueueTask(ctx, store.QueueTaskParams{QueuedAt: sql.NullString{String: now(), Valid: true}, ID: r.task.ID, FromState: "working"})
	if err != nil || queued == 0 {
		return err
	}
	j := job{
		task:        r.task,
		title:       r.title,
		branch:      r.task.Branch.String,
		pullRequest: r.pullRequest,
		prompt: fmt.Sprintf("%s\n%s# Brief\n\n%s\n\n# Issue\n\n#%d %s\n\n%s\n\n%s\n# Open items\n%s",
			implementerPrompt, sections, brief, r.task.Issue, issue.GetTitle(), issue.GetBody(), comments, r.items),
		parent: r.parent,
	}
	worker := ImplementerRole
	if r.failedCheck {
		worker = checkRoundWorker
	}
	e.runPreparedWorker(j.task, &j.title, "Implementer", false, func(ctx context.Context) (bool, error) {
		if err := e.setWorker(ctx, r.task.ID, worker, j.prompt); err != nil {
			return false, err
		}
		if err := resumeWork(ctx, repository, r.task); err != nil {
			return false, err
		}
		if r.failedCheck {
			e.addWork(r.task.ID, pullRequestWork(repository, r.pullRequest))
		}
		return true, nil
	}, func(ctx context.Context) error { return e.implementer(ctx, &j) })
	return nil
}

// resumeWork replaces mobius:review of the issue of a task in ready_for_review with mobius:working, because a round
// starts. task.State is the state before the round.
func resumeWork(ctx context.Context, repository github.Repository, task store.Task) error {
	if task.State != "ready_for_review" {
		return nil
	}
	if err := repository.AddLabel(ctx, task.Issue, workingLabel); err != nil {
		return err
	}
	return repository.RemoveLabel(ctx, task.Issue, reviewLabel)
}

// makeDraft makes the pull request a draft, so that a pull request that is ready for review is a draft during the round.
func makeDraft(ctx context.Context, repository github.Repository, pullRequest *gh.PullRequest) error {
	if pullRequest.GetDraft() {
		return nil
	}
	if err := repository.ConvertToDraft(ctx, pullRequest.GetNodeID()); err != nil {
		return err
	}
	pullRequest.Draft = new(true)
	return nil
}

// conflictRound starts a conflict round of the Implementer on the pull request of the task in task.State, which is
// ready_for_review, checks, approval or needs_human. It stores the head of the pull request in conflict_head, so that
// the poll starts no automatic round of a task in needs_human on the same head again. A round that fails stores the
// head that it pushed. The pull request is a draft during the round. The prompt has the issue body, and the round makes no change other than
// the merge of the base branch (Mobius-rust#227).
func (e *Engine) conflictRound(ctx context.Context, repository github.Repository, task store.Task, pullRequest *gh.PullRequest) error {
	brief, err := brief(ctx, repository, task.Workstream)
	if err != nil {
		return err
	}
	sections, err := e.repositorySections(ctx, repository, ImplementerRole)
	if err != nil {
		return err
	}
	issue, err := existingIssue(ctx, repository, task.Issue)
	if err != nil {
		return err
	}
	comments, err := e.readPullRequestComments(ctx, repository, int64(pullRequest.GetNumber()))
	if err != nil {
		return err
	}
	parent, err := e.newestSession(ctx, task)
	if err != nil {
		return err
	}
	if err := makeDraft(ctx, repository, pullRequest); err != nil {
		return err
	}
	queued, err := e.queries.QueueTask(ctx, store.QueueTaskParams{QueuedAt: sql.NullString{String: now(), Valid: true}, ID: task.ID, FromState: task.State})
	if err != nil || queued == 0 {
		return err
	}
	head := sql.NullString{String: pullRequest.GetHead().GetSHA(), Valid: true}
	if err := e.queries.SetTaskConflictHead(ctx, store.SetTaskConflictHeadParams{ConflictHead: head, ID: task.ID}); err != nil {
		return err
	}
	e.publishReadyForReview(task.State, "queued")
	if err := resumeWork(ctx, repository, task); err != nil {
		return err
	}
	j := job{
		task:          task,
		title:         issue.GetTitle(),
		branch:        task.Branch.String,
		pullRequest:   pullRequest,
		conflictRound: true,
		prompt: fmt.Sprintf("%s\n%s# Brief\n\n%s\n\n# Issue\n\n#%d %s\n\n%s\n\n%s\n# Base branch\n\norigin/%s\n\nMerge the base branch and remove the conflicts. Make no other change.",
			implementerPrompt, sections, brief, task.Issue, issue.GetTitle(), issue.GetBody(), comments, repository.DefaultBranch),
		parent: parent,
	}
	if err := e.setWorker(ctx, task.ID, conflictRoundWorker, j.prompt); err != nil {
		return err
	}
	e.addWork(task.ID, pullRequestWork(repository, pullRequest))
	e.runImplementer(j)
	return nil
}

// restartImplementer starts the Implementer of the queued or working task again with the same prompt, after a
// restart of the server, or after the poll found it with no Worker (lost). The task keeps its place in the queue. An
// error in the steps before the session starts the Worker again.
func (e *Engine) restartImplementer(repository github.Repository, task store.Task, lost bool) {
	if !task.WorkerInput.Valid {
		return
	}
	var j job
	e.runPreparedWorker(task, &j.title, "Implementer", lost, func(ctx context.Context) (bool, error) {
		issue, err := existingIssue(ctx, repository, task.Issue)
		if err != nil {
			return false, err
		}
		j.title = issue.GetTitle()
		var pullRequest *gh.PullRequest
		if task.PullRequest.Valid {
			if pullRequest, err = repository.PullRequest(ctx, task.PullRequest.Int64); err != nil {
				return false, err
			}
		}
		parent, err := e.restartParent(ctx, task, ImplementerRole)
		if err != nil {
			return false, err
		}
		queued, err := e.queries.RequeueTask(ctx, task.ID)
		if err != nil || queued == 0 {
			return false, err
		}
		j = job{
			task:          task,
			title:         j.title,
			branch:        task.Branch.String,
			pullRequest:   pullRequest,
			conflictRound: task.Worker.String == conflictRoundWorker,
			prompt:        task.WorkerInput.String,
			parent:        parent,
		}
		return true, nil
	}, func(ctx context.Context) error { return e.implementer(ctx, &j) })
}

func (e *Engine) setWorker(ctx context.Context, task int64, worker, input string) error {
	return e.queries.SetTaskWorker(ctx, store.SetTaskWorkerParams{
		Worker:      sql.NullString{String: worker, Valid: true},
		WorkerInput: sql.NullString{String: input, Valid: true},
		ID:          task,
	})
}

// runImplementer runs the job in the background until the task stops. After a failure, the Implementer starts again
// after the wait of RestartWorker.
func (e *Engine) runImplementer(j job) {
	e.runWorker(j.task, &j.title, "Implementer", false, func(ctx context.Context) error { return e.implementer(ctx, &j) })
}

// runPreparedWorker runs work like runWorker, after prepare. prepare gives false when the task stopped, and then work
// does not run. After an error of prepare, prepare runs again before work. After its success, only work runs again.
func (e *Engine) runPreparedWorker(task store.Task, title *string, name string, lost bool, prepare func(context.Context) (bool, error), work func(context.Context) error) {
	prepared := false
	e.runWorker(task, title, name, lost, func(ctx context.Context) error {
		if !prepared {
			ok, err := prepare(ctx)
			if err != nil || !ok {
				return err
			}
			prepared = true
		}
		return work(ctx)
	})
}

// runWorker runs work, the Worker name of the task, in the background until the task stops. title points to the title
// of the issue of the task, which is empty until work reads it. An error of work tells that the Worker must start
// again: after the wait of RestartWorker, the queued or working task goes to the queue and work runs again. A task in
// another state, for example after a decline, does not start again. A Worker that a newer Worker replaced, for example
// the Reviewer after it started a fix round, does not start again, also when it started during the wait.
//
// lost tells that the poll found the task with no Worker. Then runWorker starts only when no Worker of the task runs.
// The first run of work reads the task again, because a Worker can end between the poll and the start. When the task is
// still queued or working, that run is an error, so that the restart counts and waits like the restart after any other
// error. Otherwise the Worker ends with no count.
func (e *Engine) runWorker(task store.Task, title *string, name string, lost bool, work func(context.Context) error) {
	begin := e.startNumberedWorker
	if lost {
		begin = e.startIdleWorker
		run := work
		work = func(ctx context.Context) error {
			work = run
			current, err := e.queries.GetLiveTask(ctx, store.GetLiveTaskParams{Repository: task.Repository, Issue: task.Issue})
			if errors.Is(err, sql.ErrNoRows) || err == nil && (current.ID != task.ID || current.State != "queued" && current.State != "working") {
				return nil
			}
			if err != nil {
				return err
			}
			return errWorkerLost
		}
	}
	begin(task.ID, func(ctx context.Context, start int) {
		for {
			err := work(ctx)
			if err == nil {
				return
			}
			log.Printf("%s of %s#%d: %v", name, task.Repository, task.Issue, err)
			if e.startedAfter(task.ID, start) {
				return
			}
			again, err := e.RestartWorker(ctx, task, *title, err)
			if err == nil && again {
				if e.startedAfter(task.ID, start) {
					return
				}
				again, err = e.queueAgain(ctx, task.ID)
			}
			if err != nil && ctx.Err() == nil {
				log.Printf("restart of %s#%d: %v", task.Repository, task.Issue, err)
			}
			if err != nil || !again {
				return
			}
		}
	})
}

// queueAgain puts a working task at the end of the queue, and keeps the place of a queued task. It tells if the task
// was working or queued.
func (e *Engine) queueAgain(ctx context.Context, task int64) (bool, error) {
	queued, err := e.queries.QueueTask(ctx, store.QueueTaskParams{QueuedAt: sql.NullString{String: now(), Valid: true}, ID: task, FromState: "working"})
	if err == nil && queued == 0 {
		queued, err = e.queries.RequeueTask(ctx, task)
	}
	return queued > 0, err
}

// implementer runs one Implementer session of the job, and acts on its outcome. The end of ctx stops the session.
// An error tells that the Worker must start again.
func (e *Engine) implementer(ctx context.Context, j *job) error {
	task := j.task
	spec := Spec{
		Role:         ImplementerRole,
		Organization: owner(task.Repository),
		Repository:   task.Repository,
		Workstream:   task.Workstream,
		Issue:        sql.NullInt64{Int64: task.Issue, Valid: true},
		Parent:       j.parent,
		Task:         task.ID,
		Dir:          runner.TaskDir(e.config.DataDir, task.Repository, task.Issue),
	}
	if j.pullRequest != nil {
		spec.PullRequest = int64(j.pullRequest.GetNumber())
	}
	a, err := e.addAgent(ctx, spec)
	if err != nil {
		if errors.Is(err, ErrLeftQueue) || ctx.Err() != nil {
			return nil
		}
		return err
	}
	ended := context.WithoutCancel(ctx)
	r, err := e.implement(ctx, a, j)
	switch {
	case ctx.Err() != nil:
		return a.End(ended, "stopped")
	case errors.Is(err, errHung):
		return e.endHungTask(ended, a, task, j.title)
	case err != nil:
		return a.Fail(ended, err)
	}
	if err := a.End(ended, string(r.outcome)); err != nil {
		return err
	}
	switch r.outcome {
	case done:
		return e.review(ended, j, r.pushed, a.id)
	case cannotDo:
		// A task that the Lead declined during the turn gets no event.
		moved, err := e.setTaskState(ended, store.SetTaskStateParams{State: "dispatched", ID: task.ID, FromState: "working"})
		if err != nil || moved == 0 {
			return err
		}
		text := fmt.Sprintf("%s cannot_do on #%d \"%s\" by the Implementer:\n\n%s", time.Now().UTC().Format(timeFormat), task.Issue, j.title, quote(r.text))
		return e.addLeadEvent(ended, task.Repository, task.Workstream, sql.NullInt64{Int64: task.Issue, Valid: true}, "cannot_do", text)
	case checkFailed:
		return e.handOver(ended, task, j.title, fmt.Sprintf(".mobius/check failed %d times. Mobius pushed the work, set the Mobius check to failure, and added mobius:needs-human.", e.config.MaxCheckAttempts))
	case notMerged:
		return e.handOver(ended, task, j.title, "the conflict round did not merge the base branch. Mobius pushed the work, set the Mobius check to failure, and added mobius:needs-human.")
	case mergeConflict:
		return e.handOver(ended, task, j.title, fmt.Sprintf("the local branch diverged from origin/%s, and the merge had conflicts. Mobius pushed nothing and added mobius:needs-human. Git gave this error: %s. Resume gives the same conflict. First, in %s, merge origin/%s into the local branch and resolve the conflicts, or reset the local branch to origin/%s.", j.branch, r.text, a.spec.Dir, j.branch, j.branch))
	}
	return e.handOver(ended, task, j.title, fmt.Sprintf("GitHub rejected the push. Mobius added mobius:needs-human. Git gave this error:\n\n```\n%s\n```", r.text))
}

// stopAtLimit hands the task of the round to a human, because the pull request has open items after max_fix_rounds
// rounds of limit, for example "fix". The head of the pull request gets a failed Mobius check.
func (e *Engine) stopAtLimit(ctx context.Context, repository github.Repository, r round, limit string) error {
	handed, err := e.handToHuman(ctx, r.task)
	if err != nil || !handed {
		return err
	}
	rounds := e.config.MaxFixRounds
	summary := fmt.Sprintf("The pull request has open items after %d %s rounds.", rounds, limit)
	pullRequest, err := repository.PullRequest(ctx, int64(r.pullRequest.GetNumber()))
	if err != nil {
		return err
	}
	if err := setCheckRun(ctx, repository, pullRequest.GetHead().GetSHA(), "completed", "failure", "Round limit", summary); err != nil {
		return err
	}
	reason := fmt.Sprintf("the pull request has open items after %d %s rounds. Mobius set the Mobius check to failure and added mobius:needs-human.", rounds, limit)
	return e.addLeadEvent(ctx, r.task.Repository, r.task.Workstream, sql.NullInt64{Int64: r.task.Issue, Valid: true}, "stop", stopText(r.task.Issue, r.title, reason))
}

// handOver hands the task to a human, and gives the Lead a stop event with reason. A task that is not queued or
// working, for example after a decline, gets no event.
func (e *Engine) handOver(ctx context.Context, task store.Task, title, reason string) error {
	handed, err := e.handToHuman(ctx, task)
	if err != nil || !handed {
		return err
	}
	return e.addLeadEvent(ctx, task.Repository, task.Workstream, sql.NullInt64{Int64: task.Issue, Valid: true}, "stop", stopText(task.Issue, title, reason))
}

func stopText(number int64, title, reason string) string {
	return fmt.Sprintf("%s stop of #%d \"%s\": %s", time.Now().UTC().Format(timeFormat), number, title, reason)
}

// implement makes the worktree, runs the turns of the agent with the local check, and delivers the work.
func (e *Engine) implement(ctx context.Context, a *Agent, j *job) (result, error) {
	dataDir := e.config.DataDir
	worktree := a.spec.Dir
	prepareStartedAt := now()
	err := e.prepareWorktree(ctx, j, worktree)
	a.endStep(ctx, "prepare", prepareStartedAt, sql.NullInt64{}, err)
	if errors.Is(err, runner.ErrMergeConflict) {
		return result{outcome: mergeConflict, text: err.Error()}, nil
	} else if err != nil {
		return result{}, err
	}
	repository, err := e.repository(j.task.Repository)
	if err != nil {
		return result{}, err
	}
	// All worktrees share the refs of the bare clone, so a fetch of another task can move the base during the round
	// (Mobius-rust#228).
	baseCommit, err := runner.RevParse(ctx, dataDir, worktree, "origin/"+repository.DefaultBranch)
	if err != nil {
		return result{}, err
	}
	if err := a.open(ctx); err != nil {
		return result{}, err
	}
	e.detailsMu.Lock()
	e.implementers[j.task.ID] = a
	e.detailsMu.Unlock()
	r, failedLog, err := e.turnsAndChecks(ctx, a, j)
	e.detailsMu.Lock()
	delete(e.implementers, j.task.ID)
	e.detailsMu.Unlock()
	a.closeHarness()
	if err != nil {
		return result{}, err
	}
	if r != nil {
		return *r, nil
	}
	merged := true
	if j.conflictRound {
		if merged, err = runner.HeadContains(ctx, dataDir, worktree, baseCommit); err != nil {
			return result{}, err
		}
	}
	return e.deliver(ctx, a, j, failedLog, merged)
}

// prepareWorktree makes the worktree of the task on a new branch, or on the branch of the task when the worktree is
// gone, and merges the commits that others pushed to the branch.
func (e *Engine) prepareWorktree(ctx context.Context, j *job, worktree string) error {
	repository, err := e.repository(j.task.Repository)
	if err != nil {
		return err
	}
	token, err := repository.Token(ctx)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(worktree)
	missing := errors.Is(statErr, fs.ErrNotExist)
	login := appLogin(repository.AppSlug)
	email := ""
	if j.branch == "" || missing {
		id, err := repository.UserID(ctx, login)
		if err != nil {
			return err
		}
		email = fmt.Sprintf("%d+%s@users.noreply.github.com", id, login)
	}
	dataDir, name := e.config.DataDir, repository.FullName
	e.gitMu.Lock()
	defer e.gitMu.Unlock()
	if err := runner.Fetch(ctx, dataDir, name, repository.CloneURL, token); err != nil {
		return err
	}
	if j.branch == "" {
		branch, err := runner.FreeBranch(ctx, dataDir, name, j.task.Issue)
		if err != nil {
			return err
		}
		if err := runner.AddWorktree(ctx, dataDir, name, worktree, branch, "origin/"+repository.DefaultBranch, login, email); err != nil {
			return err
		}
		j.branch = branch
		return e.queries.SetTaskBranch(ctx, store.SetTaskBranchParams{Branch: sql.NullString{String: branch, Valid: true}, ID: j.task.ID})
	}
	if missing {
		if err := runner.AddWorktree(ctx, dataDir, name, worktree, j.branch, "origin/"+j.branch, login, email); err != nil {
			return err
		}
	}
	return runner.Pull(ctx, dataDir, worktree, j.branch)
}

// turnsAndChecks runs the turns of the agent until the local check passes. It gives a result when the Implementer
// calls cannot_do, and the end of the output of the last check when the check fails max_check_attempts times. New
// details of the Owner stop the turn that runs and go to the agent as the next prompt, with no check and no attempt
// of their own.
func (e *Engine) turnsAndChecks(ctx context.Context, a *Agent, j *job) (*result, string, error) {
	prompt := j.prompt
	attempts := 1
	for {
		err := a.Prompt(ctx, prompt, nil)
		a.mu.Lock()
		reason := a.cannotDo
		a.mu.Unlock()
		if reason != "" {
			return &result{outcome: cannotDo, text: reason}, "", nil
		}
		if details := e.takeDetails(e.implementers, j.task.ID, a, false); details != "" && ctx.Err() == nil {
			prompt = detailsPrompt("task", details)
			continue
		}
		if err != nil {
			return nil, "", err
		}
		output, passed, err := e.check(ctx, a, j, sql.NullInt64{Int64: int64(attempts), Valid: true})
		if err != nil {
			return nil, "", err
		}
		last := passed || attempts >= e.config.MaxCheckAttempts
		if details := e.takeDetails(e.implementers, j.task.ID, a, last); details != "" && ctx.Err() == nil {
			prompt = detailsPrompt("task", details)
			continue
		}
		if passed {
			return nil, "", nil
		}
		output = tail(output, logTail)
		if last {
			return nil, output, nil
		}
		attempts++
		prompt = fmt.Sprintf("The local check `.mobius/check` failed. Fix the code and commit your work. The output ends with these lines:\n\n```\n%s\n```", output)
	}
}

func detailsPrompt(subject, details string) string {
	return fmt.Sprintf("The Owner gave new details for the %s. They replace the old text where they differ.\n\n%s", subject, details)
}

// takeDetails gives the new details that wait for the session a, and clears them. With last, and no details, the
// session leaves open in the same step, so a later send finds no open session. open holds a under id.
func (e *Engine) takeDetails(open map[int64]*Agent, id int64, a *Agent, last bool) string {
	e.detailsMu.Lock()
	defer e.detailsMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	details := strings.Join(a.details, "\n\n")
	a.details = nil
	if last && details == "" {
		delete(open, id)
	}
	return details
}

// addDetails gives text as new details to the open session id of open, and ends the turn that runs. It tells if the
// session is open.
func (e *Engine) addDetails(open map[int64]*Agent, id int64, text string) bool {
	e.detailsMu.Lock()
	a, ok := open[id]
	if !ok {
		e.detailsMu.Unlock()
		return false
	}
	a.mu.Lock()
	e.detailsMu.Unlock()
	a.details = append(a.details, text)
	if a.turn {
		select {
		case a.wake <- struct{}{}:
		default:
		}
	}
	a.mu.Unlock()
	return true
}

// sendDetails gives new details of the Owner to the open Implementer session of the task. A turn that runs ends, and
// the details go to the agent as the next prompt.
func (e *Engine) sendDetails(ctx context.Context, c caller, repository github.Repository, input textInput) (string, error) {
	if empty(input.Text) {
		return "", refuse("text must not be empty.")
	}
	task, err := e.workstreamTask(ctx, repository, c.workstream, input.N)
	if err != nil {
		return "", err
	}
	if !e.addDetails(e.implementers, task.ID, input.Text) {
		if task.State == "dispatched" {
			return "", refuse("No Implementer of #%d runs. The issue body has the new details. Call `start_implementer` to continue the task.", input.N)
		}
		return "", refuse("No Implementer session of #%d is open now. A later session reads the updated issue body.", input.N)
	}
	return fmt.Sprintf("Sent the details to the Implementer of #%d.", input.N), nil
}

// check runs the local check of the worktree of a in a check slot, and gives its output and true when it passes. A
// check on a full disk waits for free space and runs again, with no new attempt (Mobius-rust#231). The session shows
// each phase of the check in its queue reason and in its Transcript (Mobius-rust#401).
func (e *Engine) check(ctx context.Context, a *Agent, j *job, attempt sql.NullInt64) (string, bool, error) {
	for {
		select {
		case e.checks <- struct{}{}:
		default:
			slotWaitedAt := now()
			err := a.checkPhase(ctx, "waits for a check slot", "The session waits for a check slot.")
			if err == nil {
				select {
				case e.checks <- struct{}{}:
				case <-ctx.Done():
					err = ctx.Err()
				}
			}
			a.endStep(ctx, "check_slot", slotWaitedAt, sql.NullInt64{}, err)
			if err != nil {
				return "", false, err
			}
		}
		err := e.waitForLowLoad(ctx, a, j)
		started := time.Now()
		if err == nil {
			err = a.checkPhase(ctx, checkRunsReason, ".mobius/check started.")
		}
		var output string
		var passed bool
		var startedAt, endedAt string
		if err == nil {
			startedAt = now()
			output, passed, err = runner.Check(ctx, e.config.DataDir, a.spec.Dir, e.agents.Path, e.config.CheckTimeout)
			if err != nil {
				a.endStep(ctx, "check", startedAt, attempt, err)
			}
		}
		endedAt = now()
		<-e.checks
		if err != nil {
			return "", false, err
		}
		elapsed := time.Since(started).Round(time.Millisecond)
		text, result := fmt.Sprintf(".mobius/check failed in %s.", elapsed), "fail"
		switch {
		case passed:
			text, result = fmt.Sprintf(".mobius/check passed in %s.", elapsed), "pass"
		case elapsed >= e.config.CheckTimeout:
			text, result = fmt.Sprintf(".mobius/check did not end in %s.", e.config.CheckTimeout), "timeout"
		}
		if err := a.addStep(ctx, "check", startedAt, endedAt, attempt, result); err != nil {
			log.Printf("add the check run of the session %d: %v", a.id, err)
		}
		if err := a.checkPhase(ctx, "", text); err != nil {
			return "", false, err
		}
		if passed || !strings.Contains(output, diskFull) {
			return output, passed, nil
		}
		if err := e.waitForDisk(ctx, j.task, j.title); err != nil {
			return "", false, err
		}
	}
}

// checkPhase shows the phase of the check of the session in its queue reason, or no queue reason for an empty phase,
// and adds a check row with text to its Transcript.
func (a *Agent) checkPhase(ctx context.Context, phase, text string) error {
	e := a.engine
	var session store.Session
	var err error
	if phase == "" {
		session, err = e.queries.ClearQueueReason(ctx, a.id)
	} else {
		session, err = e.queries.SetQueueReason(ctx, store.SetQueueReasonParams{QueueReason: sql.NullString{String: phase, Valid: true}, ID: a.id})
	}
	if err != nil {
		return err
	}
	e.publish(Change{Node: new(e.node(session))})
	row, err := compact(map[string]string{"text": text})
	if err != nil {
		return err
	}
	return e.addRow(ctx, a.id, "check", row, false)
}

// deliver pushes the work and records it on the pull request. After a passed check, a failed try repeats only these
// steps, with no new session, after the wait of RestartWorker (Mobius-rust#402). A push that GitHub rejects does not
// repeat (Mobius-rust#400).
func (e *Engine) deliver(ctx context.Context, a *Agent, j *job, failedLog string, merged bool) (result, error) {
	for {
		r, err := e.push(ctx, a, j, failedLog, merged)
		if err == nil || failedLog != "" {
			return r, err
		}
		again, restartErr := e.RestartWorker(ctx, j.task, j.title, err)
		if restartErr != nil || !again {
			return result{}, errors.Join(err, restartErr)
		}
	}
}

// push merges the commits that others pushed to the branch (Mobius-rust#229), runs the local check again when the merge
// brought new commits, pushes, opens the draft pull request of a new ticket, posts the held replies, and adds the
// Mobius check run of the head. Each try uses the installation token of the last poll (Mobius-rust#398).
func (e *Engine) push(ctx context.Context, a *Agent, j *job, failedLog string, merged bool) (result, error) {
	dataDir, worktree := e.config.DataDir, a.spec.Dir
	repository, err := e.repository(j.task.Repository)
	if err != nil {
		return result{}, err
	}
	token, err := repository.Token(ctx)
	if err != nil {
		return result{}, err
	}
	before, err := runner.RevParse(ctx, dataDir, worktree, "HEAD")
	if err != nil {
		return result{}, err
	}
	e.gitMu.Lock()
	pullStartedAt := now()
	err = runner.Fetch(ctx, dataDir, repository.FullName, repository.CloneURL, token)
	if err == nil {
		err = runner.Pull(ctx, dataDir, worktree, j.branch)
	}
	a.endStep(ctx, "pull", pullStartedAt, sql.NullInt64{}, err)
	e.gitMu.Unlock()
	if errors.Is(err, runner.ErrMergeConflict) {
		return result{outcome: mergeConflict, text: err.Error()}, nil
	}
	if err != nil {
		return result{}, err
	}
	after, err := runner.RevParse(ctx, dataDir, worktree, "HEAD")
	if err != nil {
		return result{}, err
	}
	if failedLog == "" && after != before {
		output, passed, err := e.check(ctx, a, j, sql.NullInt64{})
		if err != nil {
			return result{}, err
		}
		if !passed {
			failedLog = tail(output, logTail)
		}
	}
	oldHead := ""
	if j.pullRequest != nil {
		current, err := repository.PullRequest(ctx, int64(j.pullRequest.GetNumber()))
		if err != nil {
			return result{}, err
		}
		oldHead = current.GetHead().GetSHA()
	}
	e.gitMu.Lock()
	pushStartedAt := now()
	head, err := runner.Push(ctx, dataDir, worktree, token, j.branch)
	a.endStep(ctx, "push", pushStartedAt, sql.NullInt64{}, err)
	e.gitMu.Unlock()
	if err != nil && strings.Contains(err.Error(), "[remote rejected]") {
		return result{outcome: pushRejected, text: err.Error()}, nil
	}
	if err != nil {
		return result{}, err
	}
	if j.pullRequest == nil {
		pullRequest, err := repository.CreateDraftPullRequest(ctx, j.title, j.branch, repository.DefaultBranch, fmt.Sprintf("Workstream:\n- #%d\n\nIssue:\n- #%d\n\nCloses #%d", j.task.Workstream, j.task.Issue, j.task.Issue))
		if err != nil {
			return result{}, err
		}
		j.pullRequest = pullRequest
		if err := e.queries.SetTaskPullRequest(ctx, store.SetTaskPullRequestParams{PullRequest: sql.NullInt64{Int64: int64(pullRequest.GetNumber()), Valid: true}, ID: j.task.ID}); err != nil {
			return result{}, err
		}
		// The agents page shows the pull request of the session.
		session, err := e.queries.GetSession(ctx, a.id)
		if err != nil {
			return result{}, err
		}
		e.publish(Change{Node: new(e.node(session))})
	}
	if oldHead != head && oldHead != "" {
		if err := replaceCheckRun(ctx, repository, oldHead); err != nil {
			return result{}, err
		}
	}
	if err := a.postReplies(ctx, repository, int64(j.pullRequest.GetNumber())); err != nil {
		return result{}, err
	}
	if j.conflictRound && (failedLog != "" || !merged) {
		if err := e.queries.SetTaskConflictHead(ctx, store.SetTaskConflictHeadParams{ConflictHead: sql.NullString{String: head, Valid: true}, ID: j.task.ID}); err != nil {
			return result{}, err
		}
	}
	if failedLog == "" && !merged {
		summary := fmt.Sprintf("The Implementer did not merge `origin/%s`.", repository.DefaultBranch)
		return result{outcome: notMerged}, setCheckRun(ctx, repository, head, "completed", "failure", "Conflict round failed", summary)
	}
	if failedLog != "" {
		summary := fmt.Sprintf("`.mobius/check` failed %d times. The last output ends with these lines:\n\n```\n%s\n```", e.config.MaxCheckAttempts, failedLog)
		return result{outcome: checkFailed, text: failedLog}, setCheckRun(ctx, repository, head, "completed", "failure", "Local check failed", summary)
	}
	return result{outcome: done, pushed: pushed{j.pullRequest, head}}, setCheckRun(ctx, repository, head, "in_progress", "", "", "")
}

// postReplies posts the held replies of the Implementer on the pull request. A posted reply leaves the list, so a later
// try posts only the others.
func (a *Agent) postReplies(ctx context.Context, repository github.Repository, pullRequest int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for len(a.replies) > 0 {
		if err := a.replies[0].target.post(ctx, repository, pullRequest, a.replies[0].text); err != nil {
			return err
		}
		a.replies = a.replies[1:]
	}
	return nil
}

// brief gives the Brief of the Workstream: the body of its issue.
func brief(ctx context.Context, repository github.Repository, workstream int64) (string, error) {
	issue, err := repository.Issue(ctx, workstream)
	if err != nil {
		return "", err
	}
	if issue == nil {
		return "", errors.New("the Workstream issue does not exist")
	}
	return issue.GetBody(), nil
}

// existingIssue gives the issue number, or a refusal when it does not exist.
func existingIssue(ctx context.Context, repository github.Repository, number int64) (*gh.Issue, error) {
	issue, err := repository.Issue(ctx, number)
	if err == nil && issue == nil {
		return nil, refuse("#%d does not exist.", number)
	}
	return issue, err
}

// newestSession gives the newest session of the issue of the task.
func (e *Engine) newestSession(ctx context.Context, task store.Task) (sql.NullInt64, error) {
	return e.lastSession(ctx, task, func(store.Session) bool { return true })
}

// restartParent gives the parent of the newest session of role of the issue of the task. A session that replaces an
// ended session has the same parent.
func (e *Engine) restartParent(ctx context.Context, task store.Task, role string) (sql.NullInt64, error) {
	session, err := e.lastSession(ctx, task, func(session store.Session) bool { return session.Role == role })
	if err != nil || !session.Valid {
		return sql.NullInt64{}, err
	}
	found, err := e.queries.GetSession(ctx, session.Int64)
	return found.Parent, err
}

// lastSession gives the newest session of the issue of the task that matches.
func (e *Engine) lastSession(ctx context.Context, task store.Task, matches func(store.Session) bool) (sql.NullInt64, error) {
	sessions, err := e.queries.ListSessions(ctx, store.ListSessionsParams{Organization: owner(task.Repository), Repository: task.Repository, Workstream: task.Workstream})
	if err != nil {
		return sql.NullInt64{}, err
	}
	for i := len(sessions) - 1; i >= 0; i-- {
		if sessions[i].Issue == (sql.NullInt64{Int64: task.Issue, Valid: true}) && matches(sessions[i]) {
			return sql.NullInt64{Int64: sessions[i].ID, Valid: true}, nil
		}
	}
	return sql.NullInt64{}, nil
}

// owner gives the owner of the repository "owner/name".
func owner(repository string) string {
	organization, _, _ := strings.Cut(repository, "/")
	return organization
}
