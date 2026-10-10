package db

import (
	"github.com/kunchenguid/no-mistakes/internal/types"
	"testing"
)

func TestReviewedRecoveryStampRefusesChangedTupleOrLane(t *testing.T) {
	for _, kind := range []string{"exact", "head", "review", "status", "push", "newer", "published", "submitted"} {
		t.Run(kind, func(t *testing.T) {
			d := openTestDB(t)
			repo, _ := d.InsertRepo("/fixture", "/remote", "main")
			run, _ := d.InsertRun(repo.ID, "feature", "submitted", "base")
			if err := d.UpdateRunStatusWithVerifiedHead(run.ID, types.RunFailed, "reviewed"); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunReviewApprovedHeadSHA(run.ID, "reviewed"); err != nil {
				t.Fatal(err)
			}
			expected, _ := d.GetRun(run.ID)
			switch kind {
			case "head":
				d.UpdateRunHeadSHA(run.ID, "other")
			case "review":
				d.UpdateRunReviewApprovedHeadSHA(run.ID, "other")
			case "status":
				d.UpdateRunStatus(run.ID, types.RunRunning)
			case "push":
				d.SetRunPushActive(run.ID, true)
			case "newer":
				d.InsertRun(repo.ID, "feature", "other", "base")
			case "published":
				d.UpdateRunPushBinding(run.ID, PushBinding{HeadSHA: "other", TargetKind: "upstream", TargetFingerprint: "fingerprint", Ref: "refs/heads/feature"})
			case "submitted":
				expected.SubmittedHeadSHA = nil
			}
			updated, err := d.SetReviewedRunCustodyReturned(expected)
			if err != nil {
				t.Fatal(err)
			}
			if updated != (kind == "exact") {
				t.Fatalf("conditional update=%v", updated)
			}
			actual, _ := d.GetRun(run.ID)
			if (actual.CustodyReturnedAt != nil) != updated {
				t.Fatal("unexpected stamp")
			}
		})
	}
}

func TestReviewedRecoveryStampAtomicallyFencesEveryPushInTheLane(t *testing.T) {
	for _, kind := range []string{"selected running", "selected fixing", "older flag", "older running", "older fixing", "foreign branch"} {
		t.Run(kind, func(t *testing.T) {
			d := openTestDB(t)
			repo, err := d.InsertRepo("/fixture", "/remote", "main")
			if err != nil {
				t.Fatal(err)
			}
			older, err := d.InsertRun(repo.ID, "feature", "submitted", "base")
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(older.ID, types.RunFailed); err != nil {
				t.Fatal(err)
			}
			selected, err := d.InsertRun(repo.ID, "feature", "submitted", "base")
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatusWithVerifiedHead(selected.ID, types.RunFailed, "reviewed"); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunReviewApprovedHeadSHA(selected.ID, "reviewed"); err != nil {
				t.Fatal(err)
			}
			expected, err := d.GetRun(selected.ID)
			if err != nil {
				t.Fatal(err)
			}
			owner := selected.ID
			if kind == "older flag" || kind == "older running" || kind == "older fixing" {
				owner = older.ID
			}
			if kind == "foreign branch" {
				foreign, err := d.InsertRun(repo.ID, "other", "submitted", "base")
				if err != nil {
					t.Fatal(err)
				}
				owner = foreign.ID
			}
			if kind == "older flag" {
				if err := d.SetRunPushActive(owner, true); err != nil {
					t.Fatal(err)
				}
			} else {
				step, err := d.InsertStepResult(owner, types.StepPush)
				if err != nil {
					t.Fatal(err)
				}
				status := types.StepStatusRunning
				if kind == "selected fixing" || kind == "older fixing" {
					status = types.StepStatusFixing
				}
				if err := d.UpdateStepStatus(step.ID, status); err != nil {
					t.Fatal(err)
				}
			}
			updated, err := d.SetReviewedRunCustodyReturned(expected)
			if err != nil {
				t.Fatal(err)
			}
			if updated != (kind == "foreign branch") {
				t.Fatalf("unsettled lane push stamped custody=%v", updated)
			}
		})
	}
}
