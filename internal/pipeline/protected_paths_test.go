package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type protectedPathReconcilerStep struct {
	reconcilingApprovalStep
}

func (s *protectedPathReconcilerStep) Execute(*StepContext) (*StepOutcome, error) {
	return nil, &ProtectedPathError{Path: "package.lock", Rule: "*.lock"}
}

func TestProtectedPathRefusalRequiresDecisionAcrossRecovery(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		name := "live"
		if recovered {
			name = "recovered"
		}
		t.Run(name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			step := &protectedPathReconcilerStep{reconcilingApprovalStep: reconcilingApprovalStep{name: types.StepCI}}
			step.resolved.Store(true)
			if recovered {
				if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
				sr, err := database.InsertStepResult(run.ID, step.Name())
				if err != nil {
					t.Fatal(err)
				}
				if err := database.StartStep(sr.ID); err != nil {
					t.Fatal(err)
				}
				_, refusal := step.Execute(nil)
				outcome := ProtectedPathOutcome(refusal)
				if _, err := database.InsertStepRound(sr.ID, 1, "initial", &outcome.Findings, nil, 1); err != nil {
					t.Fatal(err)
				}
				if err := database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 1, &outcome.Findings); err != nil {
					t.Fatal(err)
				}
				run, err = database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			parked := make(chan struct{}, 1)
			exec := NewExecutor(database, p, nil, nil, []Step{step}, func(event ipc.Event) {
				if event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
					parked <- struct{}{}
				}
			})
			exec.SetGateReconcileTimings(time.Millisecond, time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("executor did not stop")
				}
			})
			workDir := t.TempDir()
			go func() {
				if recovered {
					done <- exec.Resume(ctx, run, repo, workDir)
				} else {
					done <- exec.Execute(ctx, run, repo, workDir)
				}
			}()
			select {
			case <-parked:
			case <-time.After(5 * time.Second):
				t.Fatal("refusal did not park")
			}
			if err := exec.Respond(step.Name(), types.ActionApprove, nil); err == nil || !strings.Contains(err.Error(), "use fix") {
				t.Fatalf("approval must preserve the refusal gate and explain how to retry: %v", err)
			}
			select {
			case err := <-done:
				done <- err
				t.Fatalf("refusal completed without a decision: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			if calls := step.calls.Load(); calls != 0 {
				t.Errorf("refusal invoked unrelated gate reconciliation %d times", calls)
			}
			stored, err := database.GetRun(run.ID)
			if err != nil || stored.Status != types.RunRunning || stored.AwaitingAgentSince == nil {
				t.Fatalf("refusal did not remain parked: run=%+v err=%v", stored, err)
			}
		})
	}
}
