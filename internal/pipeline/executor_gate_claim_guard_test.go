package pipeline

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Greptile on #1697: a parked gate that reconciles on its own must not resume
// once the daemon has marked a restart due; it stays parked so the restart
// keeps it, and claims the gate once the guard allows again.
func TestExecutor_GateClaimGuardKeepsAReconciledGateParked(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &reconcilingApprovalStep{name: types.StepCI}
	step.resolved.Store(true)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.SetGateReconcileTimings(10*time.Millisecond, 100*time.Millisecond)
	var allow atomic.Bool
	var released atomic.Int64
	exec.SetGateClaimGuard(func() (func(), bool) {
		if !allow.Load() {
			return nil, false
		}
		return func() { released.Add(1) }, true
	})

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)

	deadline := time.Now().Add(3 * time.Second)
	for step.calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if step.calls.Load() < 3 {
		t.Fatal("gate was not reconciled repeatedly while the guard refused")
	}
	if !exec.AwaitingApproval() {
		t.Fatal("a refused claim resumed the gate; it should stay parked")
	}
	select {
	case err := <-done:
		t.Fatalf("run finished while the guard refused the claim: %v", err)
	default:
	}

	allow.Store(true)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("gate was not claimed once the guard allowed it")
	}
	if released.Load() != 1 {
		t.Fatalf("guard released %d times, want once for the one claim", released.Load())
	}
}
