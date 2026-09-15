package db

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTestApprovalReasonMigrationAndLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo("/synthetic", "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec("ALTER TABLE step_results DROP COLUMN approval_reason"); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := legacy.GetStepResult(step.ID)
	if err != nil || got.ApprovalReason != nil || got.TestOverrideReason() != "" {
		t.Fatalf("legacy read = %+v, %v", got, err)
	}
	legacy.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	const reason = "Operator's exact reason\nwith a second line"
	findings := `{"findings":[{"id":"test-1","severity":"error","description":"failed"}]}`
	for _, reset := range []string{"revalidate", "fix", "skip"} {
		t.Run(reset, func(t *testing.T) {
			if err := d.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 7, 10, &findings); err != nil {
				t.Fatal(err)
			}
			if err := d.SetTestApprovalReason(step.ID, reason); err != nil {
				t.Fatal(err)
			}
			if err := d.CompleteStep(step.ID, 7, 10, "test.log"); err != nil {
				t.Fatal(err)
			}
			reader, err := OpenReadOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err = reader.GetStepResult(step.ID)
			reader.Close()
			if err != nil || got.ApprovalReason == nil || *got.ApprovalReason != reason || !strings.Contains(got.TestOverrideReason(), reason) || got.ExitCode == nil || *got.ExitCode != 7 || got.FindingsJSON == nil || *got.FindingsJSON != findings {
				t.Fatalf("durable completion = %+v, %v", got, err)
			}
			switch reset {
			case "revalidate":
				err = d.ResetStepsFrom(run.ID, types.StepTest.Order())
			case "fix":
				err = d.StartStepFixRound(step.ID, 1)
			case "skip":
				err = d.CompleteStepWithStatus(step.ID, types.StepStatusSkipped, 7, 10, "test.log")
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err = d.GetStepResult(step.ID)
			if err != nil || got.ApprovalReason != nil || got.TestOverrideReason() != "" {
				t.Fatalf("stale approval after %s = %+v, %v", reset, got, err)
			}
		})
	}
	if err := d.SetTestApprovalReason("missing-step", reason); err == nil {
		t.Fatal("missing step silently accepted an approval reason")
	}
}
