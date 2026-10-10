package engine_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Mobius-Toolkit/Mobius/internal/config"
	"github.com/Mobius-Toolkit/Mobius/internal/engine"
	"github.com/Mobius-Toolkit/Mobius/internal/testkit"
	"github.com/Mobius-Toolkit/Mobius/internal/testkit/testserver"
)

// longGrace is a review_quiet_period that a test does not reach. A head with no CI stays in checks.
func longGrace(cfg *config.Config) { cfg.ReviewQuietPeriod = time.Hour }

// checksHead dispatches #41, waits until its task waits for CI, and gives the head of its branch.
func checksHead(t *testing.T, server *testserver.Server, fake *testkit.FakeGitHub) string {
	t.Helper()
	fake.AddLabel(shop, 41, "mobius:ready", "owner")
	testkit.WaitFor(t, func() bool { return taskState(t, server) == "checks" })
	return head(t, fake, "mobius/41")
}

func TestATaskWaitsInChecksForTheCIOfTheHeadAndThenWaitsForTheLead(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadStarts, commits, longGrace)
	sha := checksHead(t, server, fake)
	id := fake.AddCheckRun(shop, checkRun("build", sha, "in_progress", ""))
	waitForPolls(t, fake)

	if state := taskState(t, server); state != "checks" {
		t.Errorf("state = %s", state)
	}
	if !fake.PullRequests(shop)[0].Draft {
		t.Error("the pull request is ready for review")
	}
	if want := []testkit.CheckRun{{Name: "Mobius", HeadSHA: sha, Status: "in_progress"}}; !slices.Equal(fake.CheckRuns(shop)[:1], want) {
		t.Errorf("check runs = %+v", fake.CheckRuns(shop))
	}
	if events := readyEvents(t, server); events != 0 {
		t.Errorf("events = %d", events)
	}
	if items := inbox(t, server); len(items) != 0 {
		t.Errorf("Inbox = %+v", items)
	}

	fake.SetCheckRunStatus(id, "completed", "success")

	waitForLeadPrompt(t, server, " ready for Lead approval of #41 \"Add plan model\": pull request #42 https://github.com/owner/shop/pull/42.")
	waitForPolls(t, fake)
	if state := taskState(t, server); state != "approval" {
		t.Errorf("state = %s", state)
	}
	if events := readyEvents(t, server); events != 1 {
		t.Errorf("events = %d", events)
	}
	if !fake.PullRequests(shop)[0].Draft {
		t.Error("the pull request is ready for review")
	}
	if want := (testkit.CheckRun{Name: "Mobius", HeadSHA: sha, Status: "in_progress"}); fake.CheckRuns(shop)[0] != want {
		t.Errorf("check runs = %+v", fake.CheckRuns(shop))
	}
	if items := inbox(t, server); len(items) != 0 {
		t.Errorf("Inbox = %+v", items)
	}
}

func TestAHeadWithNoCheckRunOfAnotherAppAndNoWorkflowRunStaysInChecksUntilTheQuietPeriodEnds(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadStarts, commits, longGrace)
	checksHead(t, server, fake)

	waitForPolls(t, fake)

	if state := taskState(t, server); state != "checks" {
		t.Errorf("state = %s", state)
	}
	if events := readyEvents(t, server); events != 0 {
		t.Errorf("events = %d", events)
	}
}

func TestAHeadWithNoCIGivesTheLeadEventAfterTheQuietPeriod(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadStarts, commits, func(cfg *config.Config) { cfg.ReviewQuietPeriod = 300 * time.Millisecond })
	fake.AddLabel(shop, 41, "mobius:ready", "owner")

	waitForReadyEvents(t, server, 1)

	if state := taskState(t, server); state != "approval" {
		t.Errorf("state = %s", state)
	}
}

func TestAWorkflowRunThatIsNotCompletedKeepsTheTaskInChecks(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadStarts, commits, longGrace)
	sha := checksHead(t, server, fake)
	workflow := fake.AddWorkflowRun(shop, testkit.WorkflowRun{HeadSHA: sha, Status: "queued"})
	waitForPolls(t, fake)

	if state := taskState(t, server); state != "checks" {
		t.Errorf("state = %s", state)
	}

	fake.AddCheckRun(shop, checkRun("build", sha, "completed", "success"))
	waitForPolls(t, fake)

	if state := taskState(t, server); state != "checks" {
		t.Errorf("state = %s", state)
	}

	fake.SetWorkflowRunStatus(workflow, "completed", "success")

	waitForReadyEvents(t, server, 1)
	if state := taskState(t, server); state != "approval" {
		t.Errorf("state = %s", state)
	}
}

func TestAWorkflowRunThatANewerRunOfTheSameWorkflowReplacedDoesNotCount(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadStarts, commits, longGrace)
	sha := checksHead(t, server, fake)

	cancelled := fake.AddWorkflowRun(shop, testkit.WorkflowRun{HeadSHA: sha, WorkflowID: 7, Status: "in_progress"})
	succeeded := fake.AddWorkflowRun(shop, testkit.WorkflowRun{HeadSHA: sha, WorkflowID: 7, Status: "in_progress"})
	fake.SetWorkflowRunStatus(cancelled, "completed", "cancelled")
	fake.SetWorkflowRunStatus(succeeded, "completed", "success")

	waitForReadyEvents(t, server, 1)
	if state := taskState(t, server); state != "approval" {
		t.Errorf("state = %s", state)
	}
}

func TestACompletedWorkflowRunIsEnoughCIForTheLead(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadStarts, commits, longGrace)
	sha := checksHead(t, server, fake)

	fake.AddWorkflowRun(shop, testkit.WorkflowRun{HeadSHA: sha, Status: "completed", Conclusion: "success"})

	waitForReadyEvents(t, server, 1)
}

func TestACheckRunOfAnotherHeadDoesNotCountForTheHead(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadStarts, commits, longGrace)
	checksHead(t, server, fake)

	fake.AddCheckRun(shop, checkRun("build", "0000000000000000000000000000000000000000", "completed", "success"))
	fake.AddWorkflowRun(shop, testkit.WorkflowRun{HeadSHA: "0000000000000000000000000000000000000000", Status: "completed", Conclusion: "success"})
	waitForPolls(t, fake)

	if state := taskState(t, server); state != "checks" {
		t.Errorf("state = %s", state)
	}
}

func TestAFailedCheckRunInChecksStartsAFixRoundAndGivesTheLeadNoEvent(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadStarts, fixes, longGrace)
	sha := checksHead(t, server, fake)
	fake.AddCheckRun(shop, checkRun("lint", sha, "completed", "success"))

	fake.AddCheckRun(shop, checkRun("build", sha, "completed", "failure"))

	round := roundPrompt(t, server)
	if want := "Check run \"build\""; !strings.Contains(round, want) {
		t.Errorf("%q is not in %s", want, round)
	}
	testkit.WaitFor(t, func() bool { return taskState(t, server) == "checks" && head(t, fake, "mobius/41") != sha })
	waitForPolls(t, fake)
	if events := readyEvents(t, server); events != 0 {
		t.Errorf("events = %d", events)
	}
	if task := liveTask(t, server, 41); task.FixRounds != 1 {
		t.Errorf("fix rounds = %d", task.FixRounds)
	}
}

func TestAFailedCheckRunOnTheHeadOfAFixRoundThatMadeNoCommitHandsTheTaskToAHuman(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadStarts, fixesNothing, longGrace)
	sha := checksHead(t, server, fake)
	fake.AddCheckRun(shop, checkRun("build", sha, "completed", "failure"))
	testkit.WaitFor(t, func() bool { return implementers(t, server) == 2 })
	endedImplementers(t, server, 2)

	testkit.WaitFor(t, func() bool { return taskState(t, server) == "needs_human" })
	assertCIStop(t, server, fake, sha)
}

// assertCIStop checks the labels, the Mobius check and the stop event of a task that waited in checks for a CI that
// failed.
func assertCIStop(t *testing.T, server *testserver.Server, fake *testkit.FakeGitHub, sha string) {
	t.Helper()
	testkit.WaitFor(t, func() bool {
		return slices.ContainsFunc(leadEvents(t, server), func(event leadEvent) bool { return event.Kind == "stop" })
	})
	if labels := fake.Labels(shop, 41); !slices.Equal(labels, []string{"mobius:needs-human"}) {
		t.Errorf("labels = %q", labels)
	}
	runs := mobiusCheckRuns(fake)
	if len(runs) != 1 || runs[0].HeadSHA != sha || runs[0].Conclusion != "failure" {
		t.Errorf("check runs = %+v", runs)
	}
	var stops []leadEvent
	for _, event := range leadEvents(t, server) {
		if event.Kind == "stop" {
			stops = append(stops, event)
		}
	}
	if len(stops) != 1 || !strings.Contains(stops[0].Payload, " stop of #41 \"Add plan model\": the CI of the head commit failed") {
		t.Errorf("events = %+v", stops)
	}
	if events := readyEvents(t, server); events != 0 {
		t.Errorf("events = %d", events)
	}
}

func TestAFailedWorkflowRunWithNoCheckRunHandsTheTaskToAHuman(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadStarts, commits, longGrace)
	sha := checksHead(t, server, fake)

	fake.AddWorkflowRun(shop, testkit.WorkflowRun{HeadSHA: sha, Status: "completed", Conclusion: "startup_failure"})

	testkit.WaitFor(t, func() bool { return taskState(t, server) == "needs_human" })
	assertCIStop(t, server, fake, sha)
}

func TestAPullRequestWithAnUnknownMergeabilityStaysInChecks(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadStarts, commits, longGrace)
	sha := checksHead(t, server, fake)
	fake.SetMergeableUnknown(shop, 42, true)
	waitForPolls(t, fake)
	fake.AddCheckRun(shop, checkRun("build", sha, "completed", "success"))
	waitForPolls(t, fake)

	if state := taskState(t, server); state != "checks" {
		t.Errorf("state = %s", state)
	}
	if events := readyEvents(t, server); events != 0 {
		t.Errorf("events = %d", events)
	}

	fake.SetMergeableUnknown(shop, 42, false)

	waitForReadyEvents(t, server, 1)
}

func TestAFailedCheckRunInApprovalStartsAFixRound(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadStarts, fixes, noChange)
	fake.AddLabel(shop, 41, "mobius:ready", "owner")
	waitForReadyEvents(t, server, 1)
	sha := head(t, fake, "mobius/41")

	fake.AddCheckRun(shop, checkRun("build", sha, "completed", "failure"))

	roundPrompt(t, server)
	testkit.WaitFor(t, func() bool { return taskState(t, server) == "approval" && head(t, fake, "mobius/41") != sha })
	waitForReadyEvents(t, server, 2)
}

func TestAMergeConflictInChecksStartsAConflictRoundAndGivesTheLeadNoEvent(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadStarts, mergesCents, longGrace)
	sha := checksHead(t, server, fake)
	fake.AddCheckRun(shop, checkRun("build", sha, "in_progress", ""))

	fake.CommitFile(shop, "plan.txt", "dollars\n", "Use dollars")

	testkit.WaitFor(t, func() bool { return implementers(t, server) == 2 })
	testkit.WaitFor(t, func() bool { return taskState(t, server) == "checks" && head(t, fake, "mobius/41") != sha })
	isAncestor(t, fake, sha, head(t, fake, "mobius/41"))
	waitForPolls(t, fake)
	if events := readyEvents(t, server); events != 0 {
		t.Errorf("events = %d", events)
	}
	if task := liveTask(t, server, 41); task.FixRounds != 0 {
		t.Errorf("fix rounds = %d", task.FixRounds)
	}
}

func TestAMergeConflictInApprovalStartsAConflictRound(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, first := startReady(t, fake, "", mergesCents)

	fake.CommitFile(shop, "plan.txt", "dollars\n", "Use dollars")

	waitForReadyEvents(t, server, 2)
	isAncestor(t, fake, first, head(t, fake, "mobius/41"))
}

func TestACommentInChecksGoesOnlyToTheJudgeAndTheTaskReturnsToChecks(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, "[[prompts]]\nwhen = \"You are the Judge\"\nshell = \"true\"\n\n"+leadStarts, commits, func(cfg *config.Config) { cfg.ReviewQuietPeriod = time.Second })
	sha := checksHead(t, server, fake)
	fake.AddCheckRun(shop, checkRun("build", sha, "in_progress", ""))

	fake.AddComment(shop, 42, "owner", "Why cents?")

	testkit.WaitFor(t, func() bool {
		return slices.ContainsFunc(judgePrompts(t, server), func(prompt string) bool { return strings.Contains(prompt, "Why cents?") })
	})
	testkit.WaitFor(t, func() bool {
		judges := roleSessions(t, server, engine.JudgeRole)
		return len(judges) == 1 && judges[0].EndedAt.Valid
	})
	testkit.WaitFor(t, func() bool { return taskState(t, server) == "checks" })
	waitForPolls(t, fake)
	if slices.ContainsFunc(leadPrompts(t, server), func(prompt string) bool { return strings.Contains(prompt, "Why cents?") }) {
		t.Errorf("Lead prompts = %q", leadPrompts(t, server))
	}
}

// A task that a restart finds in checks or approval has no worker. The poll continues from the state in the store.
func seedWaiting(t *testing.T, fake *testkit.FakeGitHub, state string) (*testserver.Server, string) {
	t.Helper()
	return seedWaitingWith(t, fake, state, "")
}

// seedWaitingWith is seedWaiting with the script of the fake agent after the options.
func seedWaitingWith(t *testing.T, fake *testkit.FakeGitHub, state, script string) (*testserver.Server, string) {
	t.Helper()
	dataDir := t.TempDir()
	fake.AddIssue(shop, 12, "Integrate loyalty plans")
	fake.AddLabel(shop, 12, "mobius:workstream", "owner")
	fake.AddIssue(shop, 41, "Add plan model")
	fake.AddSubIssue(shop, 12, 41)
	fake.AddLabel(shop, 41, "mobius:working", testkit.AppSlug+"[bot]")
	testkit.InstallFakeAgent(t, dataDir, options+script)
	seed(t, dataDir,
		`INSERT INTO tasks (id, repository, issue, workstream, state, dispatched_at, state_at, branch, pull_request)
		 VALUES (1, 'owner/shop', 41, 12, '`+state+`', '2026-10-04T10:00:00Z', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), 'mobius/41', 42)`)
	cfg := testserver.Config(t, dataDir)
	cfg.ReviewQuietPeriod = time.Hour
	server := startServerWith(t, fake, cfg, "")
	fake.PushCommit(shop, "mobius/41", "Add plan model")
	if number := fake.OpenPullRequest(shop, "Add plan model", "mobius/41"); number != 42 {
		t.Fatalf("pull request = %d", number)
	}
	server.WaitForFirstPoll(t, shop)
	return server, head(t, fake, "mobius/41")
}

func TestARestartKeepsTheWaitOfATaskInChecks(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, sha := seedWaiting(t, fake, "checks")
	id := fake.AddCheckRun(shop, checkRun("build", sha, "in_progress", ""))
	waitForPolls(t, fake)

	if state := taskState(t, server); state != "checks" {
		t.Errorf("state = %s", state)
	}
	if events := readyEvents(t, server); events != 0 {
		t.Errorf("events = %d", events)
	}

	fake.SetCheckRunStatus(id, "completed", "success")

	waitForReadyEvents(t, server, 1)
	if state := taskState(t, server); state != "approval" {
		t.Errorf("state = %s", state)
	}
}

func TestARestartKeepsTheWaitOfATaskInApprovalAndGivesNoSecondEvent(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, sha := seedWaiting(t, fake, "approval")
	fake.AddCheckRun(shop, checkRun("build", sha, "completed", "success"))

	waitForPolls(t, fake)

	if state := taskState(t, server); state != "approval" {
		t.Errorf("state = %s", state)
	}
	if events := readyEvents(t, server); events != 0 {
		t.Errorf("events = %d", events)
	}
}

func TestStartFixRoundWorksWhileTheTaskWaitsForTheLead(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, noFinding+leadFindings+leadStarts, commits, noChange)
	fake.AddLabel(shop, 41, "mobius:ready", "owner")
	testkit.WaitFor(t, func() bool { return taskState(t, server) == "approval" })

	sendChat(t, server, leadChat, "Send the findings to #41")

	waitForChat(t, server, leadChat, "Lead", "Sent the findings to a fix round of #41. At max_fix_rounds, Mobius stops the task instead.")
	testkit.WaitFor(t, func() bool { return implementers(t, server) == 2 })
	if task := liveTask(t, server, 41); task.FixRounds != 1 {
		t.Errorf("task = %+v", task)
	}
}

const leadApproves = "[[prompts]]\nwhen = \"Approve #41\"\ncall = { tool = \"approve_pull_request\", arguments = { n = 41 } }\n\n"

func TestApprovePullRequestMakesTheTaskReadyForReviewAndGivesTheOwnerTheInboxItem(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadApproves+leadStarts, commits, noChange)
	fake.AddLabel(shop, 41, "mobius:ready", "owner")
	waitForReadyEvents(t, server, 1)
	sha := head(t, fake, "mobius/41")

	sendChat(t, server, leadChat, "Approve #41")

	waitForChat(t, server, leadChat, "Lead", "Approved pull request #42 of #41. The Owner got it for review.")
	if state := taskState(t, server); state != "ready_for_review" {
		t.Errorf("state = %s", state)
	}
	if fake.PullRequests(shop)[0].Draft {
		t.Error("the pull request is a draft")
	}
	if want := (testkit.CheckRun{Name: "Mobius", HeadSHA: sha, Status: "completed", Conclusion: "success"}); fake.CheckRuns(shop)[0] != want {
		t.Errorf("check runs = %+v", fake.CheckRuns(shop))
	}
	want := []inboxItem{{Kind: "ready for review", Repository: shop, Workstream: 12, Issue: 41, Text: "Pull request #42 of #41 \"Add plan model\" is ready for review.", Link: "https://github.com/owner/shop/pull/42"}}
	items := inbox(t, server)
	if len(items) != 1 {
		t.Fatalf("Inbox = %+v", items)
	}
	items[0].ID = 0
	if items[0] != want[0] {
		t.Errorf("Inbox = %+v", items)
	}
}

func TestAWorkstreamsChangeGoesOutWhenATaskEntersAndLeavesReadyForReview(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadApproves+leadFindings+leadStarts, commits, noChange)
	fake.AddLabel(shop, 41, "mobius:ready", "owner")
	waitForReadyEvents(t, server, 1)
	if list := workstreams(t, server); list[0].ReadyToMerge {
		t.Fatalf("workstreams = %+v", list)
	}
	changes := listen(t, server)

	sendChat(t, server, leadChat, "Approve #41")

	waitForReadyToMerge(t, server, changes, true)
	waitForChat(t, server, leadChat, "Lead", "Approved pull request #42 of #41. The Owner got it for review.")

	sendChat(t, server, leadChat, "Send the findings to #41")

	waitForReadyToMerge(t, server, changes, false)
}

// waitForReadyToMerge reads the Workstream list after each change of the list until the first Workstream has ready.
func waitForReadyToMerge(t *testing.T, server *testserver.Server, changes <-chan engine.Change, ready bool) {
	t.Helper()
	for {
		waitForWorkstreams(t, changes)
		if workstreams(t, server)[0].ReadyToMerge == ready {
			return
		}
	}
}

func TestApprovePullRequestRefusesATaskInChecksAndChangesNothing(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadApproves+leadStarts, commits, longGrace)
	sha := checksHead(t, server, fake)

	sendChat(t, server, leadChat, "Approve #41")

	waitForChat(t, server, leadChat, "Lead", "error: The task of #41 still waits for CI, so it is not ready for approval.")
	if state := taskState(t, server); state != "checks" {
		t.Errorf("state = %s", state)
	}
	if !fake.PullRequests(shop)[0].Draft {
		t.Error("the pull request is ready for review")
	}
	if want := []testkit.CheckRun{{Name: "Mobius", HeadSHA: sha, Status: "in_progress"}}; !slices.Equal(fake.CheckRuns(shop), want) {
		t.Errorf("check runs = %+v", fake.CheckRuns(shop))
	}
	if items := inbox(t, server); len(items) != 0 {
		t.Errorf("Inbox = %+v", items)
	}
}

func TestStartFixRoundWorksWhileTheTaskWaitsForCI(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, noFinding+leadFindings+leadStarts, commits, longGrace)
	checksHead(t, server, fake)

	sendChat(t, server, leadChat, "Send the findings to #41")

	waitForChat(t, server, leadChat, "Lead", "Sent the findings to a fix round of #41. At max_fix_rounds, Mobius stops the task instead.")
	testkit.WaitFor(t, func() bool { return implementers(t, server) == 2 })
	if task := liveTask(t, server, 41); task.FixRounds != 1 {
		t.Errorf("task = %+v", task)
	}
}

func TestApprovePullRequestRefusesANewHeadAndMovesTheTaskBackToChecks(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadApproves+leadStarts, commits, keepSessionOpen)
	fake.AddLabel(shop, 41, "mobius:ready", "owner")
	waitForReadyEvents(t, server, 1)
	work := t.TempDir()
	testkit.Git(t, work, "clone", "--branch=mobius/41", fake.Remote(shop), ".")
	testkit.Git(t, work, "commit", "--allow-empty", "-m", "Change by a human")
	testkit.Git(t, work, "push", "origin", "HEAD:refs/heads/mobius/41")

	sendChat(t, server, leadChat, "Approve #41")

	waitForChat(t, server, leadChat, "Lead", "error: The head of the pull request of #41 changed after the CI passed. The task waits for the CI of the new head.")
	if !fake.PullRequests(shop)[0].Draft {
		t.Error("the pull request is ready for review")
	}
	if items := inbox(t, server); len(items) != 0 {
		t.Errorf("Inbox = %+v", items)
	}
	waitForReadyEvents(t, server, 2)
	if state := taskState(t, server); state != "approval" {
		t.Errorf("state = %s", state)
	}
}

func TestTheTasksTabAndListTasksShowTheWaitForCIAndTheWaitForTheLeadAndTheWaitForStartImplementer(t *testing.T) {
	t.Parallel()
	for state, want := range map[string]string{"checks": "waits for CI", "approval": "waits for Lead", "dispatched": "waits for start_implementer"} {
		t.Run(state, func(t *testing.T) {
			fake := testkit.NewFakeGitHub(t)
			server, _ := seedWaitingWith(t, fake, state, "[[prompts]]\ncall = { tool = \"list_tasks\" }\n")

			session, _ := leadReply(t, server)

			testkit.WaitFor(t, func() bool { return len(taskTab(t, server)) > 0 })
			lines := taskTab(t, server)
			if len(lines) != 1 || lines[0].Number != 41 || lines[0].State != want {
				t.Errorf("lines = %+v", lines)
			}
			calls := rows(t, server, session, "mcp_call")
			if len(calls) != 1 || calls[0]["result"] != "#41 Add plan model: "+want+"\n" {
				t.Errorf("calls = %+v", calls)
			}
		})
	}
}
