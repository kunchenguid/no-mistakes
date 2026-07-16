package pipeline

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

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
