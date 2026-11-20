package db

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func operatorReasonFixture(t *testing.T) (*DB, *Run, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	repo, err := d.InsertRepo("/synthetic", "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	return d, run, path
}

func approvedTestStep(t *testing.T, d *DB, run *Run, findings string, reason *string) *StepResult {
	t.Helper()
	step, err := d.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 1, 10, &findings); err != nil {
		t.Fatal(err)
	}
	if reason != nil {
		if err := d.SetTestApprovalReason(step.ID, *reason); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.CompleteStep(step.ID, 1, 10, "test.log"); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A Test exception reads the operator's recorded reason; an approval recorded
// without one - every record from before reasons were required, and the
// surfaces that take none - reads the fallback, and TestOverrideReason keeps
// its existing text for it.
func TestTestExceptionReadsTheRecordedReasonOrTheFallback(t *testing.T) {
	d, run, _ := operatorReasonFixture(t)
	noGo := `{"findings":[{"id":"test-1","severity":"error","description":"failed"}],"verdict":"no-go"}`
	reason, blank := "provider sandbox is down", ""

	recorded := approvedTestStep(t, d, run, noGo, &reason)
	if condition, got := recorded.TestException(); condition != "live validation verdict: no-go" || got != reason {
		t.Fatalf("TestException() = %q, %q", condition, got)
	}

	legacy := approvedTestStep(t, d, run, noGo, &blank)
	if condition, got := legacy.TestException(); condition == "" || got != NoOperatorReasonSupplied {
		t.Fatalf("legacy TestException() = %q, %q; want the fallback reason", condition, got)
	}
	if got, want := legacy.TestOverrideReason(), "live validation verdict: no-go\nTest exception approved: no operator reason supplied"; got != want {
		t.Fatalf("legacy TestOverrideReason() = %q, want %q", got, want)
	}

	noSurface := approvedTestStep(t, d, run, `{"findings":[],"verdict":"no-surface"}`, &reason)
	if condition, got := noSurface.TestException(); condition != "" || got != "" {
		t.Fatalf("a no-surface approval read as an exception: %q, %q", condition, got)
	}
}

// An operator skip keeps its reason through a carried copy; an automatic skip
// is never an operator skip.
func TestOperatorSkipRecordsTheReasonApartFromAutomaticSkips(t *testing.T) {
	d, run, _ := operatorReasonFixture(t)
	skip := func(name types.StepName, complete func(id string) error) *StepResult {
		t.Helper()
		step, err := d.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := complete(step.ID); err != nil {
			t.Fatal(err)
		}
		got, err := d.GetStepResult(step.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	withReason := skip(types.StepReview, func(id string) error { return d.CompleteOperatorSkippedStep(id, 0, 0, "", "docs-only change") })
	if reason, ok := withReason.OperatorSkip(); !ok || reason != "docs-only change" {
		t.Fatalf("OperatorSkip() = %q, %v", reason, ok)
	}
	withoutReason := skip(types.StepDocument, func(id string) error { return d.CompleteOperatorSkippedStep(id, 0, 0, "", "") })
	if reason, ok := withoutReason.OperatorSkip(); !ok || reason != NoOperatorReasonSupplied {
		t.Fatalf("reasonless OperatorSkip() = %q, %v; want the fallback", reason, ok)
	}
	automatic := skip(types.StepPR, func(id string) error { return d.CompleteSkippedStep(id, 0, 0, "", "no PR host") })
	if _, ok := automatic.OperatorSkip(); ok {
		t.Fatal("an automatic skip reads as an operator skip")
	}
	plain := skip(types.StepLint, func(id string) error {
		return d.CompleteStepWithStatus(id, types.StepStatusSkipped, 0, 0, "")
	})
	if _, ok := plain.OperatorSkip(); ok {
		t.Fatal("a skip with no recorded operator decision reads as an operator skip")
	}

	retry, err := d.InsertRun(run.RepoID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	carried, err := d.InsertCarriedStepResult(retry.ID, withReason)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetStepResult(carried.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reason, ok := got.OperatorSkip(); !ok || reason != "docs-only change" {
		t.Fatalf("carried OperatorSkip() = %q, %v", reason, ok)
	}
}

// A database from before the column reads every skip as no operator skip.
func TestOperatorSkipReasonReadsADatabaseWithoutTheColumn(t *testing.T) {
	d, run, path := operatorReasonFixture(t)
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteStepWithStatus(step.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec("ALTER TABLE step_results DROP COLUMN operator_skip_reason"); err != nil {
		t.Fatal(err)
	}
	legacy, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	got, err := legacy.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.OperatorSkip(); ok || got.Status != types.StepStatusSkipped {
		t.Fatalf("legacy read = %+v", got)
	}
}
