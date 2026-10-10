package engine_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Mobius-Toolkit/Mobius/internal/engine"
	"github.com/Mobius-Toolkit/Mobius/internal/store"
	"github.com/Mobius-Toolkit/Mobius/internal/testkit"
	"github.com/Mobius-Toolkit/Mobius/internal/testkit/testserver"
)

// hangs is an Implementer whose turn ends only at a cancel.
const hangs = "[[prompts]]\nhang = true\n"

// waitForApproval dispatches #41 and waits until its task waits for the Lead.
func waitForApproval(t *testing.T, server *testserver.Server, fake *testkit.FakeGitHub) {
	t.Helper()
	fake.AddLabel(shop, 41, "mobius:ready", "owner")
	testkit.WaitFor(t, func() bool { return taskState(t, server) == "approval" })
}

// noTaskLabels waits until #41 has no mobius:working, no mobius:review and no mobius:needs-human. Mobius removes the
// labels after it changes the state of the task.
func noTaskLabels(t *testing.T, fake *testkit.FakeGitHub) {
	t.Helper()
	testkit.WaitFor(t, func() bool {
		return !hasLabel(fake, "mobius:working") && !hasLabel(fake, "mobius:review") && !hasLabel(fake, "mobius:needs-human") && !hasLabel(fake, "mobius:question")
	})
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func TestAMergeEndsTheTaskAndKeepsTheBranch(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, dataDir := connectTask(t, fake, leadStarts, commits, noChange)
	waitForApproval(t, server, fake)
	worktree := filepath.Join(dataDir, "worktrees", "owner", "shop", "task-41")
	if !exists(t, worktree) {
		t.Fatal("the worktree is gone")
	}

	fake.MergePullRequest(shop, 42)

	testkit.WaitFor(t, func() bool { return taskState(t, server) == "" })
	noTaskLabels(t, fake)
	if exists(t, worktree) {
		t.Error("the worktree stays")
	}
	head(t, fake, "mobius/41")
	waitForLeadPrompt(t, server, " end of #41 \"Add plan model\": pull request #42 merged.")
}

func TestAMergeClosesTheOpenIssueAsCompleted(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, _ := connectTask(t, fake, leadStarts, commits, noChange)
	waitForApproval(t, server, fake)

	fake.MergePullRequest(shop, 42)

	testkit.WaitFor(t, func() bool { return taskState(t, server) == "" })
	if state, reason := fake.State(shop, 41); state != "closed" || reason != "completed" {
		t.Errorf("state = %s, %s", state, reason)
	}
}

func TestACloseOfTheIssueBeforeAPullRequestStopsTheImplementerAndEndsTheTask(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, dataDir := connectTask(t, fake, leadStarts, hangs, noChange)
	fake.AddLabel(shop, 41, "mobius:ready", "owner")
	testkit.WaitFor(t, func() bool {
		sessions := roleSessions(t, server, engine.ImplementerRole)
		return len(sessions) > 0 && sessions[0].AcpSessionID.Valid
	})

	fake.AddLabel(shop, 41, "mobius:question", testkit.AppSlug+"[bot]")
	fake.CloseIssue(shop, 41)

	testkit.WaitFor(t, func() bool { return taskState(t, server) == "" })
	if session := endedImplementers(t, server, 1)[0]; session.EndReason.String != "stopped" {
		t.Errorf("end reason = %s", session.EndReason.String)
	}
	noTaskLabels(t, fake)
	if exists(t, filepath.Join(dataDir, "worktrees", "owner", "shop", "task-41")) {
		t.Error("the worktree stays")
	}
	if len(fake.PullRequests(shop)) != 0 {
		t.Errorf("pull requests = %+v", fake.PullRequests(shop))
	}
}

func TestARemovalOfTheWorkingLabelStopsTheTask(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, dataDir := connectTask(t, fake, leadStarts, commits, noChange)
	waitForApproval(t, server, fake)
	items := len(inbox(t, server))

	fake.AddLabel(shop, 41, "mobius:question", testkit.AppSlug+"[bot]")
	fake.RemoveLabel(shop, 41, "mobius:working", "mallory")

	last := testkit.WaitForValue(t, func() (activity, bool) {
		feed := activities(t, server)
		return feed[len(feed)-1], strings.HasPrefix(feed[len(feed)-1].Text, "Stopped")
	})
	if state := taskState(t, server); state != "stopped" {
		t.Errorf("state = %s", state)
	}
	runs := fake.CheckRuns(shop)
	if len(runs) != 1 || runs[0].HeadSHA != head(t, fake, "mobius/41") || runs[0].Conclusion != "failure" || runs[0].Output.Summary != "Stopped by a label removal." {
		t.Errorf("check runs = %+v", runs)
	}
	noTaskLabels(t, fake)
	if exists(t, filepath.Join(dataDir, "worktrees", "owner", "shop", "task-41")) {
		t.Error("the worktree stays")
	}
	if len(fake.PullRequests(shop)) != 1 || len(inbox(t, server)) != items {
		t.Errorf("pull requests = %+v, Inbox = %+v", fake.PullRequests(shop), inbox(t, server))
	}
	want := activity{ID: last.ID, Repository: shop, Workstream: 12, Issue: 41, Actor: "mallory", Text: "Stopped \"Add plan model\" after a removal of mobius:working", Link: "https://github.com/owner/shop/issues/41"}
	if last != want {
		t.Errorf("activity = %+v", last)
	}
}

func TestAReadyLabelAfterAStopWithNoPullRequestStartsANewTaskOnANewBranch(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	implementer := "[[prompts]]\nwhen = \"Plans have a price.\"\nhang = true\n\n" + commits
	server, _ := connectTask(t, fake, leadStarts, implementer, noChange)
	fake.AddLabel(shop, 41, "mobius:ready", "owner")
	stopped := testkit.WaitForValue(t, func() (store.Task, bool) {
		task, err := store.New(server.DB).GetLiveTask(t.Context(), store.GetLiveTaskParams{Repository: shop, Issue: 41})
		return task, err == nil && task.Branch.Valid
	})
	fake.RemoveLabel(shop, 41, "mobius:working", "mallory")
	testkit.WaitFor(t, func() bool { return taskState(t, server) == "stopped" })
	fake.SetBody(shop, 41, "Plans have a price in cents.")

	fake.AddLabel(shop, 41, "mobius:ready", "owner")

	pullRequests := testkit.WaitForValue(t, func() ([]testkit.PullRequest, bool) {
		pullRequests := fake.PullRequests(shop)
		return pullRequests, len(pullRequests) == 1
	})
	task := liveTask(t, server, 41)
	if pullRequests[0].Head != "mobius/41-2" || task.ID == stopped.ID || task.Branch.String != "mobius/41-2" {
		t.Errorf("pull requests = %+v, task = %+v", pullRequests, task)
	}
}

func TestAReadyLabelAfterAStopContinuesThePullRequestOnTheSameBranch(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	finishes := "[[prompts]]\nwhen = \"The human continued the task.\"\nshell = \"echo done > done.txt && git add done.txt && git commit -q -m 'Finish the issue'\"\n\n" + commits
	server, _ := connectTask(t, fake, leadStarts, finishes, noChange)
	waitForApproval(t, server, fake)
	first := head(t, fake, "mobius/41")
	fake.RemoveLabel(shop, 41, "mobius:working", "mallory")
	testkit.WaitFor(t, func() bool { return taskState(t, server) == "stopped" })
	stopped := liveTask(t, server, 41)

	fake.AddLabel(shop, 41, "mobius:ready", "owner")

	round := roundPrompt(t, server)
	if !strings.Contains(round, "# Open items\n\nThe human continued the task. Finish the issue and make `.mobius/check` pass.\n") {
		t.Errorf("round = %s", round)
	}
	testkit.WaitFor(t, func() bool { return taskState(t, server) == "approval" && head(t, fake, "mobius/41") != first })
	isAncestor(t, fake, first, head(t, fake, "mobius/41"))
	if task := liveTask(t, server, 41); task.ID != stopped.ID || task.Branch != stopped.Branch {
		t.Errorf("task = %+v", task)
	}
	if state, _ := fake.State(shop, 42); len(fake.PullRequests(shop)) != 1 || state != "open" {
		t.Errorf("pull requests = %+v, state = %s", fake.PullRequests(shop), state)
	}
	if !slices.Contains(fake.Labels(shop, 41), "mobius:working") || slices.Contains(fake.Labels(shop, 41), "mobius:ready") {
		t.Errorf("labels = %v", fake.Labels(shop, 41))
	}
}

// A thread of the Reviewer stays open after a stop, so the next round gets it (Mobius-rust#253).
func TestARemovalOfNeedsHumanSendsTheOpenThreadOfTheMobiusAppToAFixRound(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, dataDir := connectTask(t, fake, leadStarts, fixes, noChange)
	waitForApproval(t, server, fake)
	thread := fake.AddReviewComment(shop, 42, 0, "mobius-test[bot]", "Store the unit.")
	testkit.InstallFakeHarness(t, dataDir, "devin", options+"[[prompts]]\nwhen = \"Action: fix\"\ncall = { tool = \"reply_thread\", arguments = { thread = "+itoa(thread)+", text = \"Stored.\" } }\n")
	if _, err := server.DB.Exec("UPDATE tasks SET state = 'needs_human' WHERE issue = 41"); err != nil {
		t.Fatal(err)
	}
	testkit.WaitFor(t, func() bool { return hasLabel(fake, "mobius:needs-human") })

	fake.RemoveLabel(shop, 41, "mobius:needs-human", "owner")

	round := roundPrompt(t, server)
	_, items, _ := strings.Cut(round, "# Open items\n")
	inOrder(t, items, "Thread "+itoa(thread)+", src/plan.rs line 12:", "Store the unit.", "Action: fix\n")
	testkit.WaitFor(t, func() bool { return taskState(t, server) == "approval" })
	if len(fake.PullRequests(shop)) != 1 {
		t.Errorf("pull requests = %+v", fake.PullRequests(shop))
	}
}
