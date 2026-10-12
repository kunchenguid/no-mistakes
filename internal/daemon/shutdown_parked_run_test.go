package daemon

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func startParkedRunDaemon(t *testing.T, p *paths.Paths, d *db.DB) chan error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWithOptions(p, d, func() []pipeline.Step {
			return []pipeline.Step{&mockApprovalStep{name: types.StepDocument}}
		})
	}()
	waitForDaemonReady(t, p)
	return errCh
}

func stopParkedRunDaemon(t *testing.T, p *paths.Paths, errCh chan error) {
	t.Helper()
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	if err := client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil); err != nil {
		t.Fatalf("shutdown daemon: %v", err)
	}
	_ = client.Close()
	select {
	case <-errCh:
	case <-time.After(35 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func newParkedRunEnv(t *testing.T) (*paths.Paths, *db.DB) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
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
	t.Cleanup(func() { _ = d.Close() })
	return p, d
}

func TestShutdown_KeepsRunParkedAtApprovalGateForResume(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "shutdown-parked-run")
	run, err := d.InsertRun(repo.ID, "main", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	worktree := p.WorktreeDir(repo.ID, run.ID)
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), worktree, headSHA); err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepDocument)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"needs approval","action":"ask-user"}],"summary":"needs approval"}`
	if err := d.SetStepFindings(step.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertReviewStepRound(step.ID, 1, "initial", &findings, nil, headSHA, 1); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateStepStatusWithDuration(step.ID, types.StepStatusAwaitingApproval, 1); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}

	// First daemon: startup recovery resumes the parked run, then a clean stop
	// cancels it exactly as SIGTERM does.
	errCh := startParkedRunDaemon(t, p, d)
	stopParkedRunDaemon(t, p, errCh)

	stopped, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status != types.RunRunning || stopped.AwaitingAgentSince == nil {
		t.Fatalf("run after clean stop = status %s awaiting %v error %v, want running and still parked", stopped.Status, stopped.AwaitingAgentSince, stopped.Error)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree of a parked run must survive a clean stop: %v", err)
	}
	steps, err := d.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Status != types.StepStatusAwaitingApproval {
		t.Fatalf("step after clean stop = %#v, want awaiting_approval", steps)
	}

	approveResumedRun(t, p, d, run.ID)
}

func approveResumedRun(t *testing.T, p *paths.Paths, d *db.DB, runID string) {
	t.Helper()
	// Second daemon resumes the same gate and accepts the approval.
	errCh := startParkedRunDaemon(t, p, d)
	defer stopParkedRunDaemon(t, p, errCh)

	deadline := time.Now().Add(5 * time.Second)
	for {
		client, err := ipc.Dial(p.Socket())
		if err == nil {
			err = client.Call(ipc.MethodRespond, &ipc.RespondParams{
				RunID:  runID,
				Step:   types.StepDocument,
				Action: types.ActionApprove,
			}, &ipc.RespondResult{})
			_ = client.Close()
			if err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("resumed gate never accepted an approval: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	completed := waitForRunTerminalState(t, d, runID)
	if completed.Status != types.RunCompleted {
		t.Fatalf("resumed run status = %s, want completed", completed.Status)
	}
}

func TestShutdown_KeepsLiveParkedRunForResume(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "shutdown-live-parked-run")

	errCh := startParkedRunDaemon(t, p, d)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	var pushed ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &pushed)
	_ = client.Close()
	if err != nil {
		t.Fatal(err)
	}
	runID := pushed.RunID

	deadline := time.Now().Add(60 * time.Second)
	for {
		steps, _ := d.GetStepsByRun(runID)
		if len(steps) == 1 && steps[0].Status == types.StepStatusAwaitingApproval {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("step never reached awaiting_approval")
		}
		time.Sleep(50 * time.Millisecond)
	}
	run, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	for run.AwaitingAgentSince == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if run, err = d.GetRun(runID); err != nil {
			t.Fatal(err)
		}
	}

	stopParkedRunDaemon(t, p, errCh)

	stopped, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status != types.RunRunning || stopped.AwaitingAgentSince == nil {
		t.Fatalf("run after clean stop = status %s awaiting %v error %v, want running and still parked", stopped.Status, stopped.AwaitingAgentSince, stopped.Error)
	}
	if _, err := os.Stat(p.WorktreeDir(repo.ID, runID)); err != nil {
		t.Fatalf("worktree of a parked run must survive a clean stop: %v", err)
	}
	steps, err := d.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Status != types.StepStatusAwaitingApproval {
		t.Fatalf("step after clean stop = %#v, want awaiting_approval", steps)
	}

	approveResumedRun(t, p, d, runID)
}
