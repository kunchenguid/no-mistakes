package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestResponseOriginMigratesOldRowsAsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo("/test/origin", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"R1","description":"repair"}]}`
	round, err := d.InsertStepRound(step.ID, 1, "initial", &findings, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["R1"]`
	if err := d.SetStepRoundSelection(round.ID, &selected, RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"step_results", "step_rounds"} {
		if _, err := legacy.Exec("ALTER TABLE " + table + " DROP COLUMN response_origin"); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopening twice also proves migrations remain idempotent.
	for range 2 {
		d, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := d.GetStepResult(step.ID)
		if err != nil || string(got.ResponseOrigin) != "unknown" {
			t.Fatalf("legacy step = %+v, err %v", got, err)
		}
		rounds, err := d.GetRoundsByStep(step.ID)
		if err != nil || len(rounds) != 1 || string(rounds[0].ResponseOrigin) != "unknown" {
			t.Fatalf("legacy rounds = %+v, err %v", rounds, err)
		}
		decisions, _, err := d.GetBranchDecisionRounds(repo.ID, run.Branch, "other-run", 10)
		if err != nil || len(decisions) != 1 || string(decisions[0].Round.ResponseOrigin) != "unknown" {
			t.Fatalf("legacy decisions = %+v, err %v", decisions, err)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResponseOriginDoesNotInferFromSelectionSource(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/test/origin", "https://example.com/repo.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	round, err := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := `["R1"]`
	if err := d.SetStepRoundUserDecision(round.ID, &ids, RoundSelectionSourceUser, nil); err != nil {
		t.Fatal(err)
	}
	rounds, err := d.GetRoundsByStep(step.ID)
	if err != nil || string(rounds[0].ResponseOrigin) != "unknown" {
		t.Fatalf("inferred origin: %+v, %v", rounds, err)
	}
	if err := d.SetStepResponseOrigin("missing-step", types.ResponseOrigin("auto_yes")); err == nil {
		t.Fatal("missing decision owner accepted")
	}
	if err := d.SetStepResponseOrigin(step.ID, types.ResponseOrigin("interactive")); err != nil {
		t.Fatal(err)
	}
	if err := d.ResetStepsFrom(run.ID, 0); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetStepResult(step.ID)
	if err != nil || string(got.ResponseOrigin) != "unknown" {
		t.Fatalf("reset retained stale origin: %+v, %v", got, err)
	}
}
