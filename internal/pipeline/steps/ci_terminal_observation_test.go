package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCITerminalObservationDoesNotCompletePipeline(t *testing.T) {
	for _, state := range []string{"MERGED", "CLOSED"} {
		for _, reconcile := range []bool{false, true} {
			name := state
			if reconcile {
				name += "-reconcile"
			}
			t.Run(name, func(t *testing.T) {
				dir, base, head := setupGitRepo(t)
				sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
				sctx.Env = fakeCIGH(t, state, "[]")
				prURL := "https://github.com/test/repo/pull/42"
				sctx.Run.PRURL = &prURL
				if err := sctx.DB.UpdateRunStatus(sctx.Run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
				ci, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
				if err != nil {
					t.Fatal(err)
				}
				if err := sctx.DB.StartStep(ci.ID); err != nil {
					t.Fatal(err)
				}
				sctx.StepResultID = ci.ID
				step := &CIStep{}
				if reconcile {
					resolved, err := step.ReconcileApprovalGate(sctx)
					if err != nil || !resolved {
						t.Fatalf("reconcile=%t err=%v", resolved, err)
					}
				} else {
					outcome, err := step.Execute(sctx)
					if err != nil || outcome == nil || outcome.NeedsApproval {
						t.Fatalf("outcome=%+v err=%v", outcome, err)
					}
				}
				run, err := sctx.DB.GetRun(sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				result, err := sctx.DB.GetStepResult(ci.ID)
				if err != nil {
					t.Fatal(err)
				}
				if run.Status != types.RunRunning || result.Status != types.StepStatusRunning {
					t.Fatalf("terminal observation completed run=%s ci=%s before executor proof", run.Status, result.Status)
				}
				expected := "merged"
				if state == "CLOSED" {
					expected = "closed"
				}
				if sctx.Run.PRState == nil || *sctx.Run.PRState != expected {
					t.Fatalf("post-CI guard has no pending %s observation: %v", expected, sctx.Run.PRState)
				}
			})
		}
	}
}
