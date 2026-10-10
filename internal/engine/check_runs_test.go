package engine_test

import (
	"testing"
	"time"

	"github.com/Mobius-Toolkit/Mobius/internal/engine"
	"github.com/Mobius-Toolkit/Mobius/internal/testkit"
	"github.com/Mobius-Toolkit/Mobius/internal/testkit/testserver"
)

// seedOpenCheck starts a server with the task row, whose pull request #42 has a Mobius check run that is not complete.
// The Agent hangs. It gives the head of the pull request.
func seedOpenCheck(t *testing.T, fake *testkit.FakeGitHub, task string) (*testserver.Server, string) {
	t.Helper()
	dataDir := t.TempDir()
	fake.AddIssue(shop, 12, "Integrate loyalty plans")
	fake.AddLabel(shop, 12, "mobius:workstream", "owner")
	fake.AddIssue(shop, 41, "Add plan model")
	fake.AddSubIssue(shop, 12, 41)
	fake.AddLabel(shop, 41, "mobius:working", testkit.AppSlug+"[bot]")
	testkit.InstallFakeAgent(t, dataDir, options+hangs)
	seed(t, dataDir, task)
	cfg := testserver.Config(t, dataDir)
	cfg.ReviewQuietPeriod = time.Hour
	server := startServerWith(t, fake, cfg, "")
	fake.PushCommit(shop, "mobius/41", "Add plan model")
	sha := head(t, fake, "mobius/41")
	fake.AddCheckRun(shop, checkRun("Mobius", sha, "in_progress", ""))
	if number := fake.OpenPullRequest(shop, "Add plan model", "mobius/41"); number != 42 {
		t.Fatalf("pull request = %d", number)
	}
	return server, sha
}

func TestTheRestartOfTheReviewerKeepsOneMobiusCheckRunOnTheHead(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, sha := seedOpenCheck(t, fake, `INSERT INTO tasks (id, repository, issue, workstream, state, dispatched_at, queued_at, branch, pull_request, worker)
		VALUES (1, 'owner/shop', 41, 12, 'working', '2026-10-04T10:00:00Z', '2026-10-04T10:00:00Z', 'mobius/41', 42, 'reviewer')`)

	testkit.WaitFor(t, func() bool { return len(roleSessions(t, server, engine.ReviewerRole)) == 1 })

	if runs := mobiusCheckRuns(fake); len(runs) != 1 || runs[0].HeadSHA != sha || runs[0].Status != "in_progress" {
		t.Errorf("check runs = %+v", runs)
	}
}

func TestTheJudgeThatFindsNoOpenThreadKeepsOneMobiusCheckRunOnTheHead(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, sha := seedOpenCheck(t, fake, `INSERT INTO tasks (id, repository, issue, workstream, state, dispatched_at, state_at, branch, pull_request)
		VALUES (1, 'owner/shop', 41, 12, 'reviewed', '2026-10-04T10:00:00Z', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), 'mobius/41', 42)`)

	testkit.WaitFor(t, func() bool { return taskState(t, server) == "checks" })
	waitForPolls(t, fake)

	if runs := mobiusCheckRuns(fake); len(runs) != 1 || runs[0].HeadSHA != sha || runs[0].Status != "in_progress" {
		t.Errorf("check runs = %+v", runs)
	}
}

func TestTheEndOfTheWaitInChecksKeepsOneMobiusCheckRunOnTheHead(t *testing.T) {
	t.Parallel()
	fake := testkit.NewFakeGitHub(t)
	server, sha := seedOpenCheck(t, fake, `INSERT INTO tasks (id, repository, issue, workstream, state, dispatched_at, state_at, branch, pull_request)
		VALUES (1, 'owner/shop', 41, 12, 'checks', '2026-10-04T10:00:00Z', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), 'mobius/41', 42)`)
	fake.AddCheckRun(shop, checkRun("build", sha, "completed", "success"))

	testkit.WaitFor(t, func() bool { return taskState(t, server) == "approval" })

	if runs := mobiusCheckRuns(fake); len(runs) != 1 || runs[0].HeadSHA != sha || runs[0].Status != "in_progress" {
		t.Errorf("check runs = %+v", runs)
	}
}
