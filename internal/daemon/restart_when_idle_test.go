package daemon

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #1691: updating no-mistakes must not fail runs already in progress.
// A scheduled restart lets the executing run finish on the running daemon,
// holds pushes that arrive meanwhile, and the restarted daemon runs them.

// heldStep executes until the test releases it, so "a run is executing when
// the restart is scheduled" is a state the test controls rather than a race.
type heldStep struct {
	name     types.StepName
	started  chan struct{}
	once     sync.Once
	released <-chan struct{}
}

func (s *heldStep) Name() types.StepName { return s.name }
func (s *heldStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.released:
		return &pipeline.StepOutcome{}, nil
	case <-sctx.Ctx.Done():
		return nil, sctx.Ctx.Err()
	}
}

func TestScheduledRestartLetsTheExecutingRunFinishAndRunsHeldPushesAfterward(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "restart-when-idle")
	gate := p.RepoDir(repo.ID)
	release := make(chan struct{})
	step := &heldStep{name: types.StepReview, started: make(chan struct{}), released: release}
	steps := func() []pipeline.Step { return []pipeline.Step{step} }

	first := make(chan error, 1)
	go func() { first <- RunWithOptions(p, d, steps) }()
	waitForDaemonReady(t, p)

	executing := submitBranchRun(t, p, gate, "feature-a", headSHA)
	<-step.started

	var scheduled ipc.RestartStatusResult
	if err := callDaemon(p, ipc.MethodScheduleRestart, &ipc.ScheduleRestartParams{}, &scheduled); err != nil {
		t.Fatalf("schedule restart: %v", err)
	}
	if !scheduled.Pending || scheduled.WaitingRuns != 1 {
		t.Fatalf("scheduled restart = %+v, want pending and waiting for the one executing run", scheduled)
	}

	// A push during the wait is held, not started on the old daemon.
	if held := submitBranchRun(t, p, gate, "feature-b", headSHA); held != "" {
		t.Fatalf("push during the wait started run %s on the old daemon, want it held", held)
	}
	if run, err := d.GetActiveRun(repo.ID, "feature-b"); err != nil || run != nil {
		t.Fatalf("held push has a run before the restart: %+v, %v", run, err)
	}
	// A launch other than a plain push is refused, so its caller retries.
	err := callDaemon(p, ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: gate, Ref: "refs/heads/feature-c", Old: zeroSHA, New: headSHA, LaunchNonce: "nonce-1",
	}, &ipc.PushReceivedResult{})
	if err == nil || !strings.Contains(err.Error(), "restarting for an update") {
		t.Fatalf("proof-mode launch during the wait = %v, want the restart refusal", err)
	}
	var status ipc.RestartStatusResult
	if err := callDaemon(p, ipc.MethodRestartStatus, &ipc.ScheduleRestartParams{}, &status); err != nil {
		t.Fatal(err)
	}
	if status.WaitingRuns != 1 || status.HeldPushes != 1 {
		t.Fatalf("restart status = %+v, want 1 waiting run and 1 held push", status)
	}
	select {
	case err := <-first:
		t.Fatalf("daemon stopped while a run was executing: %v", err)
	default:
	}

	close(release)
	if run := waitForRunTerminalState(t, d, executing); run.Status != types.RunCompleted {
		t.Fatalf("executing run ended %s (%v), want completed on the daemon it started on", run.Status, errText(run.Error))
	}
	select {
	case err := <-first:
		if !errors.Is(err, ErrRestartForUpdate) {
			t.Fatalf("daemon stopped with %v, want ErrRestartForUpdate", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not stop for the restart once the run finished")
	}

	// The restarted daemon starts the held push.
	second := make(chan error, 1)
	go func() { second <- RunWithOptions(p, d, steps) }()
	waitForDaemonReady(t, p)
	t.Cleanup(func() { stopParkedRunDaemon(t, p, second) })
	heldRun := waitForBranchRun(t, d, repo.ID, "feature-b")
	if run := waitForRunTerminalState(t, d, heldRun.ID); run.Status != types.RunCompleted {
		t.Fatalf("held push ended %s (%v), want completed on the restarted daemon", run.Status, errText(run.Error))
	}
	if _, err := os.Stat(p.HeldPushesFile()); !os.IsNotExist(err) {
		t.Fatalf("held pushes file should be removed once replayed, stat err = %v", err)
	}
}

func TestScheduledRestartDoesNotWaitForARunParkedAtAGate(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, headSHA := setupTestGitRepo(t, p, d, "restart-parked")
	first := make(chan error, 1)
	go func() {
		first <- RunWithOptions(p, d, func() []pipeline.Step {
			return []pipeline.Step{&mockApprovalStep{name: types.StepDocument}}
		})
	}()
	waitForDaemonReady(t, p)

	runID := submitBranchRun(t, p, p.RepoDir(repo.ID), "feature-a", headSHA)
	waitForParked(t, d, runID)

	var scheduled ipc.RestartStatusResult
	if err := callDaemon(p, ipc.MethodScheduleRestart, &ipc.ScheduleRestartParams{}, &scheduled); err != nil {
		t.Fatalf("schedule restart: %v", err)
	}
	if scheduled.WaitingRuns != 0 {
		t.Fatalf("scheduled restart waits for %d runs, want 0: a parked run survives a clean stop", scheduled.WaitingRuns)
	}
	select {
	case err := <-first:
		if !errors.Is(err, ErrRestartForUpdate) {
			t.Fatalf("daemon stopped with %v, want ErrRestartForUpdate", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a run parked at a gate held the restart up")
	}
	run, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunRunning || run.AwaitingAgentSince == nil {
		t.Fatalf("parked run after the restart stop = %s (awaiting since %v), want still parked for resume", run.Status, run.AwaitingAgentSince)
	}
}

const zeroSHA = "0000000000000000000000000000000000000000"

func callDaemon(p *paths.Paths, method string, params, result any) error {
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return err
	}
	defer client.Close()
	return client.Call(method, params, result)
}

func waitForBranchRun(t *testing.T, d *db.DB, repoID, branch string) *db.Run {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := d.GetRunsByRepo(repoID)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range runs {
			if run.Branch == branch {
				return run
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no run for branch %s appeared", branch)
	return nil
}

func waitForParked(t *testing.T, d *db.DB, runID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil && run.AwaitingAgentSince != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s never parked at its gate", runID)
}

func errText(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
