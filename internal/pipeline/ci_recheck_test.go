package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

type boundedCIRecheckStep struct{ reconcilingApprovalStep }

func (*boundedCIRecheckStep) RecheckApprovalGate(sctx *StepContext, _ string) (string, error) {
	<-sctx.Ctx.Done()
	// Even a provider that returns late success cannot complete the gate.
	return "verified", nil
}

func TestExecutor_CIRecheckTimeoutPreservesGate(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &boundedCIRecheckStep{reconcilingApprovalStep: reconcilingApprovalStep{name: types.StepCI}}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.SetGateReconcileTimings(time.Hour, 20*time.Millisecond)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- exec.Execute(ctx, run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepCI, types.ActionRecheck, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("recheck error=%v", err)
	}
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepCI, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ordinary approval stopped working")
	}
}

func TestExecutor_FailedCIRecheckDoesNotStopRacingReconciliation(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &boundedCIRecheckStep{reconcilingApprovalStep: reconcilingApprovalStep{
		name: types.StepCI, started: make(chan struct{}), release: make(chan struct{}),
	}}
	step.resolved.Store(true)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.SetGateReconcileTimings(10*time.Millisecond, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	workDir := t.TempDir()
	go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
	select {
	case <-step.started:
	case <-ctx.Done():
		t.Fatal("no reconciliation")
	}
	response := make(chan error, 1)
	go func() { response <- exec.Respond(types.StepCI, types.ActionRecheck, nil) }()
	for {
		exec.mu.Lock()
		claimed := !exec.waiting
		exec.mu.Unlock()
		if claimed {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("recheck did not claim gate")
		case <-time.After(time.Millisecond):
		}
	}
	close(step.release)
	if err := <-response; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("recheck error=%v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("failed recheck stopped reconciliation timer")
	}
}

func TestCanRecheckCIProvider(t *testing.T) {
	legacy := `{"findings":[{"severity":"warning","description":"CI checks could not be read from the provider: provider unavailable. Verify that the provider CLI or credentials are installed, authenticated, and support the required check-reading command.","action":"ask-user"}]}`
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{legacy, true},
		{`{"findings":[{"category":"ci-provider-read","action":"ask-user"}]}`, true},
		{`{"findings":[]}`, false}, {`not-json`, false},
		{`{"findings":[{"id":"ci-1","description":"failed","action":"ask-user"}]}`, false},
		{`{"findings":[{"category":"ci-review-bot","action":"ask-user"}]}`, false},
		{`{"findings":[{"category":"ci-provider-read","action":"auto-fix"}]}`, false},
		{`{"findings":[{"category":"ci-provider-read","action":"ask-user","file":"code.go"}]}`, false},
	} {
		if got := CanRecheckCIProvider(tc.raw); got != tc.want {
			t.Errorf("CanRecheckCIProvider(%s)=%v", tc.raw, got)
		}
	}
}
