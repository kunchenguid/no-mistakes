package db

import (
	"github.com/kunchenguid/no-mistakes/internal/types"
	"path/filepath"
	"testing"
)

func TestAdmitLateCIFindingsFailsClosedAndRetainsRecoveryGate(t *testing.T) {
	for _, refused := range []string{"stale", "unpublished", "fixing", "closed", "push-active", "missing-step", "accepted"} {
		t.Run(refused, func(t *testing.T) {
			d, err := Open(filepath.Join(t.TempDir(), "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			repo, _ := d.InsertRepo("/tmp/repo", "origin", "main")
			run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
			step, _ := d.InsertStepResult(run.ID, types.StepCI)
			if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/1"); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunPushBinding(run.ID, PushBinding{HeadSHA: "head", TargetKind: "origin", Ref: "refs/heads/feature"}); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunReviewApprovedHeadSHA(run.ID, "head"); err != nil {
				t.Fatal(err)
			}
			if err := d.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			if err := d.SetRunCIReady(run.ID, true); err != nil {
				t.Fatal(err)
			}
			head, stepID := "head", step.ID
			switch refused {
			case "stale":
				head = "other"
			case "unpublished":
				if err := d.UpdateRunHeadSHA(run.ID, "other"); err != nil {
					t.Fatal(err)
				}
				head = "other"
			case "fixing":
				if err := d.StartStepFixRound(step.ID, 1); err != nil {
					t.Fatal(err)
				}
			case "closed":
				if err := d.UpdateRunPRState(run.ID, "closed"); err != nil {
					t.Fatal(err)
				}
			case "push-active":
				if err := d.SetRunPushActive(run.ID, true); err != nil {
					t.Fatal(err)
				}
			case "missing-step":
				stepID = "missing"
			}
			before, _ := d.GetRun(run.ID)
			raw := `{"findings":[{"id":"late-1","description":"new acceptance","severity":"error","action":"ask-user"}]}`
			err = d.AdmitLateCIFindings(run.ID, stepID, head, raw)
			after, _ := d.GetRun(run.ID)
			result, _ := d.GetStepResult(step.ID)
			if refused != "accepted" {
				if err == nil {
					t.Fatal("unsafe admission succeeded")
				}
				if (before.CIReadyAt == nil) != (after.CIReadyAt == nil) || (before.ReviewApprovedHeadSHA == nil) != (after.ReviewApprovedHeadSHA == nil) || after.AwaitingAgentSince != nil || result.FindingsJSON != nil {
					t.Fatal("refusal changed durable state")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if after.CIReadyAt != nil || after.ReviewApprovedHeadSHA != nil || after.AwaitingAgentSince == nil || result.Status != types.StepStatusAwaitingApproval || result.FindingsJSON == nil || *result.FindingsJSON != raw {
				t.Fatalf("amendment not retained: run=%+v step=%+v", after, result)
			}
			// An in-flight old provider observation cannot restore readiness.
			if err := d.SetRunCIReady(run.ID, true); err != nil {
				t.Fatal(err)
			}
			after, _ = d.GetRun(run.ID)
			if after.CIReadyAt != nil {
				t.Fatal("stale readiness restored")
			}
			if err := d.AdmitLateCIFindings(run.ID, step.ID, head, raw); err == nil {
				t.Fatal("duplicate admission succeeded")
			}
		})
	}
}
