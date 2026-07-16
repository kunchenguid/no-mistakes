package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPushReceivedReturnsWhileProvisioningQueued(t *testing.T) {
	p, d, mgr, repo, headSHA := newProvisioningTestManager(t, "provisioning-queued")
	fillProvisionSlots(t, mgr)

	type result struct {
		runID string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		runID, err := mgr.HandlePushReceived(context.Background(), &ipc.PushReceivedParams{
			Gate: p.RepoDir(repo.ID),
			Ref:  "refs/heads/main",
			Old:  "0000000000000000000000000000000000000000",
			New:  headSHA,
		})
		done <- result{runID: runID, err: err}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(750 * time.Millisecond):
		t.Fatal("push admission waited for provisioning instead of returning after queueing")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	run, err := d.GetRun(got.runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunProvisioning || run.ProvisioningPhase != "queued" {
		t.Fatalf("queued run projection = %+v", run)
	}

	releaseOneProvisionSlot(t, mgr)
	if terminal := waitForRunTerminalState(t, d, got.runID); terminal.Status != types.RunCompleted {
		t.Fatalf("terminal run = %+v", terminal)
	}
}

func TestCancelQueuedProvisioningPersistsCancelledRun(t *testing.T) {
	p, d, mgr, repo, headSHA := newProvisioningTestManager(t, "provisioning-cancel")
	fillProvisionSlots(t, mgr)

	runID, err := mgr.HandlePushReceived(context.Background(), &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.HandleCancel(runID); err != nil {
		t.Fatal(err)
	}
	run := waitForRunStatus(t, d, runID, types.RunCancelled)
	if run.Error == nil || !strings.Contains(*run.Error, types.RunCancelReasonAbortedByUser) {
		t.Fatalf("cancelled run error = %+v", run.Error)
	}
	if _, err := os.Stat(p.WorktreeDir(repo.ID, runID)); !os.IsNotExist(err) {
		t.Fatalf("queued cancellation created worktree, stat err=%v", err)
	}
}

func TestResumeProvisioningRunsRequeuesAfterRestart(t *testing.T) {
	p, d, mgr, repo, headSHA := newProvisioningTestManager(t, "provisioning-restart")
	run, err := d.InsertRun(repo.ID, "main", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunProvisioning(run.ID, "worktree", 5, ""); err != nil {
		t.Fatal(err)
	}
	partialWorktree := p.WorktreeDir(repo.ID, run.ID)
	if err := git.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), partialWorktree, headSHA); err != nil {
		t.Fatal(err)
	}

	mgr.resumeProvisioningRuns([]*db.Run{run})
	if terminal := waitForRunTerminalState(t, d, run.ID); terminal.Status != types.RunCompleted {
		t.Fatalf("terminal run = %+v", terminal)
	}
	events, err := d.LifecycleEvents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.EventType == "provisioning_completed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing provisioning_completed event: %+v", events)
	}
}

func newProvisioningTestManager(t *testing.T, repoID string) (*paths.Paths, *db.DB, *RunManager, *db.Repo, string) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mockClaude := writeMockClaude(t, t.TempDir())
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: "+mockClaude+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, headSHA := setupTestGitRepo(t, p, d, repoID)
	mgr := NewRunManager(d, p, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	t.Cleanup(func() {
		mgr.Shutdown()
		_ = d.Close()
		_ = os.RemoveAll(tmpDir)
	})
	return p, d, mgr, repo, headSHA
}

func fillProvisionSlots(t *testing.T, mgr *RunManager) {
	t.Helper()
	for i := 0; i < cap(mgr.provisionSlots); i++ {
		mgr.provisionSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for {
			select {
			case <-mgr.provisionSlots:
			default:
				return
			}
		}
	})
}

func releaseOneProvisionSlot(t *testing.T, mgr *RunManager) {
	t.Helper()
	select {
	case <-mgr.provisionSlots:
	case <-time.After(1 * time.Second):
		t.Fatal("provision slot was not held")
	}
}

func waitForRunStatus(t *testing.T, d *db.DB, runID string, want types.RunStatus) *db.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil && run.Status == want {
			return run
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach status %s", runID, want)
	return nil
}
