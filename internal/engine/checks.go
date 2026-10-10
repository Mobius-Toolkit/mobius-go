package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/Mobius-Toolkit/Mobius/internal/github"
	"github.com/Mobius-Toolkit/Mobius/internal/store"
)

const (
	// githubActions is the slug of the App of GitHub Actions.
	githubActions = "github-actions"
	// jobLogLines is the number of lines of the end of a job log of GitHub Actions in a fix round.
	jobLogLines = 200
	// longCIWait is the time in checks with CI that still runs, and the time between two events about it. A normal CI
	// run completes in much less time, and a run in waiting or action_required must not block a task for a full day
	// before the Lead knows.
	longCIWait = 2 * time.Hour
	// approvalReminder is the time in approval, and the time between two reminders. A pull request that waits for the
	// Lead approval must come back to the Lead after a short time, for fast iterations.
	approvalReminder = 2 * time.Hour
	// dispatchReminder is the time in dispatched with no open question, and the time between two reminders. A
	// dispatched task holds an Autopilot slot while no agent works on it. The value is the same as the limit in checks
	// and in approval.
	dispatchReminder = 2 * time.Hour
)

// ciWait is the head that the poll saw first at a time, for a task in checks.
type ciWait struct {
	head  string
	since time.Time
}

// failedCheckRuns gives the completed check runs of other Apps that failed on the commit sha.
func failedCheckRuns(ctx context.Context, repository github.Repository, sha string) ([]*gh.CheckRun, error) {
	runs, err := repository.CheckRuns(ctx, sha)
	if err != nil {
		return nil, err
	}
	var failed []*gh.CheckRun
	for _, run := range runs {
		if run.GetName() != checkRunName && run.GetStatus() == "completed" && failedConclusion(run.GetConclusion()) {
			failed = append(failed, run)
		}
	}
	return failed, nil
}

// unhandledFailure tells if the head of the pull request of the task has a failed check run of another App and no fix
// round yet.
func (e *Engine) unhandledFailure(ctx context.Context, repository github.Repository, task store.Task, pullRequest *gh.PullRequest) (bool, error) {
	head := pullRequest.GetHead().GetSHA()
	if task.CheckHead.String == head {
		return false, nil
	}
	runs, err := failedCheckRuns(ctx, repository, head)
	return len(runs) > 0, err
}

// ciItems gives the items of a fix round for the failed check runs of other Apps on the commit head. Each failed check
// run is an item with its annotations, and a check run of GitHub Actions also has the end of its job log.
func ciItems(ctx context.Context, repository github.Repository, head string) (string, error) {
	runs, err := failedCheckRuns(ctx, repository, head)
	if err != nil {
		return "", err
	}
	var items strings.Builder
	for _, run := range runs {
		output := run.GetOutput()
		fmt.Fprintf(&items, "\nCheck run \"%s\", %s:\n%s\n\n%s\n", run.GetName(), run.GetHTMLURL(), output.GetTitle(), output.GetSummary())
		annotations, err := repository.CheckRunAnnotations(ctx, run.GetID())
		if err != nil {
			return "", err
		}
		for _, annotation := range annotations {
			fmt.Fprintf(&items, "- %s line %d: %s\n", annotation.GetPath(), annotation.GetStartLine(), annotation.GetMessage())
		}
		if run.GetApp().GetSlug() == githubActions {
			// A job log that cannot download leaves the round with the annotations.
			if log, err := repository.JobLog(ctx, run.GetID()); err == nil {
				lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
				fmt.Fprintf(&items, "\nEnd of the job log:\n%s\n", strings.Join(lines[max(0, len(lines)-jobLogLines):], "\n"))
			}
		}
		items.WriteString("\nAction: fix\n")
	}
	return items.String(), nil
}

// onFailure starts a fix round when check runs of other Apps failed on the head of the pull request of the task in
// ready_for_review, checks or approval. A head gets one round. It gives true when a round started, or when
// the task left its state.
func (e *Engine) onFailure(ctx context.Context, repository github.Repository, task store.Task, pullRequest *gh.PullRequest) (bool, error) {
	head := pullRequest.GetHead().GetSHA()
	if task.CheckHead.String == head {
		return false, nil
	}
	items, err := ciItems(ctx, repository, head)
	if err != nil {
		return false, err
	}
	if items == "" {
		return false, nil
	}
	issue, err := existingIssue(ctx, repository, task.Issue)
	if err != nil {
		return false, err
	}
	parent, err := e.newestSession(ctx, task)
	if err != nil {
		return false, err
	}
	implementer, found := store.Session{}, false
	if task.State == "checks" {
		if implementer, found, err = e.implementerSession(ctx, task); err != nil {
			return false, err
		}
	}
	moved, err := e.setTaskState(ctx, store.SetTaskStateParams{State: "working", ID: task.ID, FromState: task.State})
	if err != nil || moved == 0 {
		return true, err
	}
	if err := e.fixRound(ctx, repository, round{task: task, title: issue.GetTitle(), pullRequest: pullRequest, counts: true, items: items, parent: parent, failedCheck: true}); err != nil {
		_, stateErr := e.setTaskState(ctx, store.SetTaskStateParams{State: task.State, ID: task.ID, FromState: "working"})
		return false, errors.Join(err, stateErr)
	}
	if found {
		e.addCIStep(ctx, task, implementer, "failure")
	}
	return true, e.queries.SetTaskCheckHead(ctx, store.SetTaskCheckHeadParams{CheckHead: sql.NullString{String: head, Valid: true}, ID: task.ID})
}

// onChecks moves the task in checks to approval when the CI of the head of its pull request passed and the pull request
// has no merge conflict, and gives the Lead the event ready_for_approval one time for the head. A task that is not in
// checks, for example after a decline, stays as it is. A pull request whose mergeability GitHub still calculates waits.
// A head whose CI failed after its fix round, or whose workflow run failed with no failed check run to fix, goes to a
// human.
func (e *Engine) onChecks(ctx context.Context, repository github.Repository, task store.Task, pullRequest *gh.PullRequest) error {
	if pullRequest.Mergeable == nil {
		return nil
	}
	head := pullRequest.GetHead().GetSHA()
	state, err := ciOf(ctx, repository, head)
	if err != nil {
		return err
	}
	switch {
	case state.failedCheck && task.CheckHead.String != head:
		return nil
	case state.failedCheck || state.failedWorkflow:
		return e.ciFailed(ctx, repository, task, pullRequest)
	case state.running:
		return e.onLongCIWait(ctx, repository, task, pullRequest, state.incomplete)
	case state.absent && !e.quietPeriodEnded(task.ID, head):
		return nil
	}
	issue, err := existingIssue(ctx, repository, task.Issue)
	if err != nil {
		return err
	}
	if err := setCheckRun(ctx, repository, head, "in_progress", "", "", ""); err != nil {
		return err
	}
	implementer, found, err := e.implementerSession(ctx, task)
	if err != nil {
		return err
	}
	moved, err := e.setTaskState(ctx, store.SetTaskStateParams{State: "approval", ID: task.ID, FromState: "checks"})
	if err != nil || moved == 0 {
		return err
	}
	if found {
		e.addCIStep(ctx, task, implementer, "success")
	}
	delete(e.ciWait, task.ID)
	text := fmt.Sprintf("%s ready for Lead approval of #%d \"%s\": pull request #%d %s.", time.Now().UTC().Format(timeFormat), task.Issue, issue.GetTitle(), pullRequest.GetNumber(), pullRequest.GetHTMLURL())
	return e.addLeadEvent(ctx, task.Repository, task.Workstream, sql.NullInt64{Int64: task.Issue, Valid: true}, "ready_for_approval", text)
}

// onLongCIWait gives the Lead the event long_ci_wait when longCIWait passed since the task entered checks, or since the
// last event of this kind. It does not change the task.
func (e *Engine) onLongCIWait(ctx context.Context, repository github.Repository, task store.Task, pullRequest *gh.PullRequest, incomplete []string) error {
	return e.remindLead(ctx, task, longCIWait, "long_ci_wait", func(now time.Time, _ time.Duration) (string, error) {
		issue, err := existingIssue(ctx, repository, task.Issue)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s the CI of #%d \"%s\" runs for more than %s: pull request #%d %s, head %s. These runs did not complete: %s.", now.UTC().Format(timeFormat), task.Issue, issue.GetTitle(), longCIWait, pullRequest.GetNumber(), pullRequest.GetHTMLURL(), pullRequest.GetHead().GetSHA(), strings.Join(incomplete, ", ")), nil
	})
}

// remindApproval gives the Lead the event approval_reminder when approvalReminder passed since the task entered
// approval, or since the last event of this kind. It does not change the task.
func (e *Engine) remindApproval(ctx context.Context, repository github.Repository, task store.Task, pullRequest *gh.PullRequest) error {
	return e.remindLead(ctx, task, approvalReminder, "approval_reminder", func(now time.Time, waited time.Duration) (string, error) {
		issue, err := existingIssue(ctx, repository, task.Issue)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s #%d \"%s\" waits for Lead approval for %s: pull request #%d %s.", now.UTC().Format(timeFormat), task.Issue, issue.GetTitle(), waited.Round(time.Minute), pullRequest.GetNumber(), pullRequest.GetHTMLURL()), nil
	})
}

// remindDispatch gives the Lead the event dispatch_reminder when dispatchReminder passed since the task entered
// dispatched, or since the last event of this kind. It does not change the task.
func (e *Engine) remindDispatch(ctx context.Context, task store.Task, issue *gh.Issue) error {
	return e.remindLead(ctx, task, dispatchReminder, "dispatch_reminder", func(now time.Time, waited time.Duration) (string, error) {
		return fmt.Sprintf("%s #%d \"%s\" waits for an Implementer for %s and holds an Autopilot slot.", now.UTC().Format(timeFormat), task.Issue, issue.GetTitle(), waited.Round(time.Minute)), nil
	})
}

// remindLead gives the Lead an event of the kind when limit passed since the task entered its state, or since the last
// event of the kind. text gets the time of the event and the time since the task entered its state.
func (e *Engine) remindLead(ctx context.Context, task store.Task, limit time.Duration, kind string, text func(now time.Time, waited time.Duration) (string, error)) error {
	entered, err := time.Parse(time.RFC3339Nano, task.StateAt)
	if err != nil {
		return err
	}
	last := entered
	if task.LongWaitAt.Valid {
		if last, err = time.Parse(time.RFC3339Nano, task.LongWaitAt.String); err != nil {
			return err
		}
	}
	now := e.timeNow()
	if now.Sub(last) < limit {
		return nil
	}
	body, err := text(now, now.Sub(entered))
	if err != nil {
		return err
	}
	if err := e.addLeadEvent(ctx, task.Repository, task.Workstream, sql.NullInt64{Int64: task.Issue, Valid: true}, kind, body); err != nil {
		return err
	}
	return e.queries.SetTaskLongWaitAt(ctx, store.SetTaskLongWaitAtParams{LongWaitAt: sql.NullString{String: now.UTC().Format(time.RFC3339Nano), Valid: true}, ID: task.ID})
}

// ciFailed hands the task in checks to a human, with a failed Mobius check and a stop event for the Lead. The task
// keeps the head in ci_failed_head, so that continueNeedsHuman can tell this stop from another stop.
func (e *Engine) ciFailed(ctx context.Context, repository github.Repository, task store.Task, pullRequest *gh.PullRequest) error {
	implementer, found, err := e.implementerSession(ctx, task)
	if err != nil {
		return err
	}
	handed, err := e.handToHuman(ctx, task)
	if err != nil || !handed {
		return err
	}
	if found {
		e.addCIStep(ctx, task, implementer, "failure")
	}
	head := sql.NullString{String: pullRequest.GetHead().GetSHA(), Valid: true}
	if err := e.queries.SetTaskCiFailedHead(ctx, store.SetTaskCiFailedHeadParams{CiFailedHead: head, ID: task.ID}); err != nil {
		return err
	}
	issue, err := existingIssue(ctx, repository, task.Issue)
	if err != nil {
		return err
	}
	if err := setCheckRun(ctx, repository, pullRequest.GetHead().GetSHA(), "completed", "failure", "CI failed", "The CI of the head commit failed, and a fix round cannot change it."); err != nil {
		return err
	}
	reason := "the CI of the head commit failed, and a fix round cannot change it. Mobius set the Mobius check to failure and added mobius:needs-human."
	return e.addLeadEvent(ctx, task.Repository, task.Workstream, sql.NullInt64{Int64: task.Issue, Valid: true}, "stop", stopText(task.Issue, issue.GetTitle(), reason))
}

// ci is what the check runs of other Apps and the workflow runs of GitHub Actions on a head show.
type ci struct {
	failedCheck, failedWorkflow, running bool
	// incomplete holds the names of the check runs and the workflow runs that did not complete.
	incomplete []string
	// absent is true for a head with no check run of another App and no workflow run: its CI has not started, or the
	// repository has no CI.
	absent bool
}

// ciOf reads the CI of the head. A workflow run can exist before its jobs have check runs.
func ciOf(ctx context.Context, repository github.Repository, head string) (ci, error) {
	runs, err := repository.CheckRuns(ctx, head)
	if err != nil {
		return ci{}, err
	}
	workflows, err := repository.WorkflowRuns(ctx, head)
	if err != nil {
		return ci{}, err
	}
	var state ci
	others := 0
	for _, run := range runs {
		if run.GetName() == checkRunName {
			continue
		}
		others++
		switch {
		case run.GetStatus() != "completed":
			state.running = true
			state.incomplete = append(state.incomplete, run.GetName())
		case failedConclusion(run.GetConclusion()):
			state.failedCheck = true
		}
	}
	for _, workflow := range newestWorkflowRuns(workflows) {
		switch {
		case workflow.GetStatus() != "completed":
			state.running = true
			state.incomplete = append(state.incomplete, workflow.GetName())
		case failedConclusion(workflow.GetConclusion()) || workflow.GetConclusion() == "startup_failure":
			state.failedWorkflow = true
		}
	}
	state.absent = others+len(workflows) == 0
	return state, nil
}

// newestWorkflowRuns keeps the run with the highest id of each workflow, as the check runs keep the latest run of each
// name. An older run that a newer run replaced, for example a cancelled one, does not count.
func newestWorkflowRuns(runs []*gh.WorkflowRun) []*gh.WorkflowRun {
	newest := map[int64]*gh.WorkflowRun{}
	for _, run := range runs {
		if current, ok := newest[run.GetWorkflowID()]; !ok || run.GetID() > current.GetID() {
			newest[run.GetWorkflowID()] = run
		}
	}
	return slices.Collect(maps.Values(newest))
}

// quietPeriodEnded tells if review_quiet_period passed since the first poll that saw the head with no CI, so a
// repository with no CI does not wait forever.
func (e *Engine) quietPeriodEnded(taskID int64, head string) bool {
	seen, ok := e.ciWait[taskID]
	if !ok || seen.head != head {
		seen = ciWait{head, time.Now()}
		e.ciWait[taskID] = seen
	}
	return time.Since(seen.since) >= e.config.ReviewQuietPeriod
}

// failedConclusion tells if the conclusion of a completed check run is a failure.
func failedConclusion(conclusion string) bool {
	return conclusion == "failure" || conclusion == "timed_out" || conclusion == "cancelled"
}
