package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_TestExceptionApprovalSurvivesRecovery(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "recovered"}[recovered], func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			findings := `{"findings":[{"id":"test-1","severity":"error","action":"ask-user","description":"synthetic failure"}]}`
			const reason = "Accepted only for this synthetic test\nwith a recorded explanation"
			step := newApprovalStep(types.StepTest, findings)
			var final ipc.Event
			executor := NewExecutor(database, p, nil, nil, []Step{step}, func(event ipc.Event) {
				if event.Type == ipc.EventRunCompleted {
					final = event
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			workDir := t.TempDir()
			if recovered {
				if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
				sr, err := database.InsertStepResult(run.ID, types.StepTest)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.StartStep(sr.ID); err != nil {
					t.Fatal(err)
				}
				if err := database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 7, 10, &findings); err != nil {
					t.Fatal(err)
				}
				if _, err := database.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 10); err != nil {
					t.Fatal(err)
				}
				run, err = database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := ValidateRecoveredRun(database, run, []Step{step}); err != nil {
					steps, _ := database.GetStepsByRun(run.ID)
					rounds, _ := database.GetRoundsByStep(sr.ID)
					record, _ := json.MarshalIndent(map[string]any{"run": run, "steps": steps, "rounds": rounds}, "", "  ")
					t.Fatalf("recovery fixture rejected: %v\n%s", err, record)
				}
				go func() { done <- executor.Resume(ctx, run, repo, workDir) }()
			} else {
				go func() { done <- executor.Execute(ctx, run, repo, workDir) }()
			}
			var err error
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
				select {
				case err := <-done:
					t.Fatalf("executor exited before approval: %v", err)
				default:
				}
				err = executor.RespondWithOverrides(types.StepTest, types.ActionApprove, nil, nil, nil, reason)
				if err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			steps, err := database.GetStepsByRun(run.ID)
			if err != nil || len(steps) != 1 {
				t.Fatalf("steps = %+v, %v", steps, err)
			}
			if steps[0].ApprovalReason == nil || *steps[0].ApprovalReason != reason || steps[0].Status != types.StepStatusCompleted {
				t.Fatalf("approval evidence = %+v", steps[0])
			}
			if steps[0].FindingsJSON == nil || !strings.Contains(*steps[0].FindingsJSON, "synthetic failure") {
				t.Fatalf("original failure lost: %+v", steps[0])
			}
			if final.TestOverrideReason == nil || !strings.Contains(*final.TestOverrideReason, reason) || final.CIOverrideReason != nil {
				t.Fatalf("completion delta = %+v", final)
			}
		})
	}
}

func TestExecutor_ApprovalReasonCannotChangeOtherActions(t *testing.T) {
	executor := NewExecutor(nil, nil, nil, nil, nil, nil)
	for _, tc := range []struct {
		step   types.StepName
		action types.ApprovalAction
	}{{types.StepReview, types.ActionApprove}, {types.StepCI, types.ActionApprove}, {types.StepTest, types.ActionFix}, {types.StepTest, types.ActionSkip}} {
		if err := executor.RespondWithOverrides(tc.step, tc.action, nil, nil, nil, "reason"); err == nil || !strings.Contains(err.Error(), "only to Test approval") {
			t.Fatalf("reason accepted for %s/%s: %v", tc.step, tc.action, err)
		}
	}
}
