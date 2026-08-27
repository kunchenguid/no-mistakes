package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type reconcilingApprovalStep struct {
	name      types.StepName
	resolved  atomic.Bool
	calls     atomic.Int64
	err       atomic.Pointer[error]
	block     bool
	started   chan struct{}
	callStart chan int64
	release   chan struct{}
	startOnce atomic.Bool
}

func (s *reconcilingApprovalStep) Name() types.StepName { return s.name }

func (s *reconcilingApprovalStep) Execute(*StepContext) (*StepOutcome, error) {
	return &StepOutcome{
		NeedsApproval: true,
		Findings:      `{"findings":[{"id":"ci-1","severity":"warning","description":"waiting","action":"ask-user"}],"summary":"waiting"}`,
	}, nil
}

func (s *reconcilingApprovalStep) ReconcileApprovalGate(sctx *StepContext) (bool, error) {
	call := s.calls.Add(1)
	if s.callStart != nil {
		s.callStart <- call
	}
	if s.startOnce.CompareAndSwap(false, true) && s.started != nil {
		close(s.started)
	}
	if s.release != nil {
		<-s.release
	}
	if s.block {
		<-sctx.Ctx.Done()
		return false, sctx.Ctx.Err()
	}
	if ptr := s.err.Load(); ptr != nil {
		return false, *ptr
	}
	return s.resolved.Load(), nil
}

func TestExecutor_AcceptedApprovalWinsReconciliationRace(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &reconcilingApprovalStep{
		name:    types.StepCI,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	step.resolved.Store(true)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.SetGateReconcileTimings(time.Hour, time.Second)

	workDir := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()
	select {
	case <-step.started:
	case <-time.After(3 * time.Second):
		t.Fatal("gate reconciliation did not start")
	}
	if err := exec.Respond(types.StepCI, types.ActionAbort, nil); err != nil {
		t.Fatalf("Respond() error = %v", err)
	}
	close(step.release)

	select {
	case err := <-done:
		if err == nil || err.Error() != "step ci: aborted by user" {
			t.Fatalf("Execute() error = %v, want accepted abort", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("accepted abort did not complete")
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunFailed {
		t.Fatalf("run status = %s, want %s", got.Status, types.RunFailed)
	}
}

func TestExecutor_ReconcilesParkedGateThroughNormalCompletionPath(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &reconcilingApprovalStep{name: types.StepCI}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.SetGateReconcileTimings(10*time.Millisecond, 100*time.Millisecond)

	workDir := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)

	step.resolved.Store(true)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reconciled gate did not complete")
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunCompleted || got.AwaitingAgentSince != nil {
		t.Fatalf("run after reconciliation = status %s awaiting %v", got.Status, got.AwaitingAgentSince)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Status != types.StepStatusCompleted {
		t.Fatalf("steps after reconciliation = %+v", steps)
	}
}

func TestExecutor_ReconcileErrorPreservesGateFailClosed(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &reconcilingApprovalStep{
		name:      types.StepCI,
		callStart: make(chan int64, 4),
		release:   make(chan struct{}),
	}
	reconcileErr := error(errors.New("provider unavailable"))
	step.err.Store(&reconcileErr)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.SetGateReconcileTimings(10*time.Millisecond, 50*time.Millisecond)

	workDir := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)

	select {
	case <-step.callStart:
	case <-time.After(3 * time.Second):
		t.Fatal("first reconcile check did not start")
	}
	step.release <- struct{}{}
	select {
	case <-step.callStart:
	case <-time.After(3 * time.Second):
		t.Fatal("second reconcile check did not start")
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunRunning || got.AwaitingAgentSince == nil {
		t.Fatalf("reconcile error changed parked run: status %s awaiting %v", got.Status, got.AwaitingAgentSince)
	}
	if step.calls.Load() < 2 {
		t.Fatalf("reconcile calls = %d, want repeated bounded checks", step.calls.Load())
	}

	if err := exec.Respond(types.StepCI, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	step.release <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute() after approval error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approval did not complete preserved gate")
	}
}

func TestExecutor_FatalReconcileErrorFailsRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &reconcilingApprovalStep{name: types.StepCI}
	reconcileErr := error(fmt.Errorf("%w: head continuity lost", ErrFatalGateReconciliation))
	step.err.Store(&reconcileErr)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.SetGateReconcileTimings(time.Millisecond, 50*time.Millisecond)

	err := exec.Execute(context.Background(), run, repo, t.TempDir())
	if !errors.Is(err, ErrFatalGateReconciliation) {
		t.Fatalf("Execute() error = %v, want fatal reconciliation error", err)
	}
	got, dbErr := database.GetRun(run.ID)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	if got.Status != types.RunFailed || got.AwaitingAgentSince != nil {
		t.Fatalf("run after fatal reconciliation = status %s awaiting %v", got.Status, got.AwaitingAgentSince)
	}
	if err := exec.Respond(types.StepCI, types.ActionApprove, nil); err == nil {
		t.Fatal("fatal reconciliation left the gate approvable")
	}
}

func TestExecutor_ResumeFatalReconcileErrorFailsRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"ci-1","severity":"warning","description":"waiting","action":"ask-user"}],"summary":"waiting"}`
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(stepResult.ID, 1, "initial", &findings, nil, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	step := &reconcilingApprovalStep{name: types.StepCI}
	reconcileErr := error(fmt.Errorf("%w: head continuity lost", ErrFatalGateReconciliation))
	step.err.Store(&reconcileErr)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	err = exec.Resume(context.Background(), run, repo, t.TempDir())
	if !errors.Is(err, ErrFatalGateReconciliation) {
		t.Fatalf("Resume() error = %v, want fatal reconciliation error", err)
	}
	got, dbErr := database.GetRun(run.ID)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	if got.Status != types.RunFailed || got.AwaitingAgentSince != nil || got.ParkedMS <= 0 {
		t.Fatalf("recovered run after fatal reconciliation = status %s awaiting %v parked_ms %d", got.Status, got.AwaitingAgentSince, got.ParkedMS)
	}
	steps, dbErr := database.GetStepsByRun(run.ID)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	if len(steps) != 1 || steps[0].Status != types.StepStatusFailed {
		t.Fatalf("recovered steps after fatal reconciliation = %+v", steps)
	}
}

func TestExecutor_GateRecheckIsBoundedAndApprovalWinsAfterTimeout(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &reconcilingApprovalStep{name: types.StepCI, block: true, started: make(chan struct{})}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.SetGateReconcileTimings(time.Hour, 25*time.Millisecond)

	workDir := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()
	select {
	case <-step.started:
	case <-time.After(3 * time.Second):
		t.Fatal("gate reconciliation did not start")
	}
	if err := exec.Respond(types.StepCI, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocking provider check was not bounded")
	}
}

// TestExecutor_AppliesGateReconcileTimingsFromGlobalConfig is the operator
// path for gate_reconcile_interval / gate_reconcile_timeout: write them in
// global config.yaml, load + merge, construct NewExecutor with that Config
// (no SetGateReconcileTimings), and prove a hanging reconcile is bounded by
// the configured timeout rather than the hardcoded 30s default. If wiring
// were missing, the blocking check would hold the gate for ~30s and this
// 1s deadline would fail.
func TestExecutor_AppliesGateReconcileTimingsFromGlobalConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	// Operator raises the per-attempt budget for slow gh auth probes; the
	// interval stays long so only the timeout bound is under test here.
	body := "gate_reconcile_interval: \"1h\"\ngate_reconcile_timeout: \"25ms\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	global, err := config.LoadGlobal(cfgPath)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if global.GateReconcileInterval != time.Hour || global.GateReconcileTimeout != 25*time.Millisecond {
		t.Fatalf("loaded timings = interval %v timeout %v, want 1h / 25ms",
			global.GateReconcileInterval, global.GateReconcileTimeout)
	}
	cfg := config.Merge(global, &config.RepoConfig{})
	if cfg.GateReconcileInterval != time.Hour || cfg.GateReconcileTimeout != 25*time.Millisecond {
		t.Fatalf("merged timings = interval %v timeout %v, want 1h / 25ms",
			cfg.GateReconcileInterval, cfg.GateReconcileTimeout)
	}

	database, p, run, repo := setupTest(t)
	step := &reconcilingApprovalStep{name: types.StepCI, block: true, started: make(chan struct{})}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	workDir := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()
	select {
	case <-step.started:
	case <-time.After(3 * time.Second):
		t.Fatal("gate reconciliation did not start")
	}
	if err := exec.Respond(types.StepCI, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("configured gate_reconcile_timeout was not applied; blocking check still used the hardcoded default")
	}
}

// TestExecutor_AppliesGateReconcileIntervalFromGlobalConfig proves the
// operator-configured interval (not the hardcoded 2m) drives how often a
// still-parked gate is rechecked. With interval 10ms, a second reconcile must
// arrive well before the 2m default; Approve ends the park cleanly.
func TestExecutor_AppliesGateReconcileIntervalFromGlobalConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "gate_reconcile_interval: \"10ms\"\ngate_reconcile_timeout: \"50ms\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	global, err := config.LoadGlobal(cfgPath)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	cfg := config.Merge(global, &config.RepoConfig{})

	database, p, run, repo := setupTest(t)
	step := &reconcilingApprovalStep{name: types.StepCI}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	workDir := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)

	deadline := time.Now().Add(500 * time.Millisecond)
	for step.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if step.calls.Load() < 2 {
		t.Fatalf("reconcile calls = %d within 500ms, want >= 2 from configured 10ms interval (default 2m would not recheck yet)", step.calls.Load())
	}

	if err := exec.Respond(types.StepCI, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("parked gate did not complete after approve")
	}
}

func TestExecutor_GateRecheckStopsAfterApprovalCancelAndShutdown(t *testing.T) {
	tests := []struct {
		name   string
		finish func(*Executor, context.CancelCauseFunc) error
	}{
		{
			name: "approval",
			finish: func(exec *Executor, _ context.CancelCauseFunc) error {
				return exec.Respond(types.StepCI, types.ActionApprove, nil)
			},
		},
		{
			name: "cancel",
			finish: func(_ *Executor, cancel context.CancelCauseFunc) error {
				cancel(errors.New(types.RunCancelReasonAbortedByUser))
				return nil
			},
		},
		{
			name: "shutdown",
			finish: func(_ *Executor, cancel context.CancelCauseFunc) error {
				cancel(errors.New("daemon shutting down"))
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			step := &reconcilingApprovalStep{name: types.StepCI}
			exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			exec.SetGateReconcileTimings(5*time.Millisecond, 50*time.Millisecond)
			ctx, cancel := context.WithCancelCause(context.Background())
			workDir := t.TempDir()
			done := make(chan error, 1)
			go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
			waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)
			deadline := time.Now().Add(time.Second)
			for step.calls.Load() < 2 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if err := tt.finish(exec, cancel); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("executor did not finish")
			}
			settled := step.calls.Load()
			time.Sleep(30 * time.Millisecond)
			if got := step.calls.Load(); got != settled {
				t.Fatalf("gate watcher leaked after %s: calls advanced from %d to %d", tt.name, settled, got)
			}
		})
	}
}
