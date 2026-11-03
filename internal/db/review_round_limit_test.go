package db

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewMaxFixRoundsMigrationAndLegacyReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo(t.TempDir(), "https://example.com/repo", "main")
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
	if _, err := d.sql.Exec("ALTER TABLE step_results DROP COLUMN review_max_fix_rounds"); err != nil {
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
	if err != nil || got.ReviewMaxFixRounds != nil {
		t.Fatalf("legacy read=%+v, %v", got, err)
	}
	if _, err := legacy.GetStepsByRun(run.ID); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.PinReviewMaxFixRounds(step.ID, 2); err != nil {
		t.Fatal(err)
	}
	got, err = d.GetStepResult(step.ID)
	if err != nil || got.ReviewMaxFixRounds == nil || *got.ReviewMaxFixRounds != 2 {
		t.Fatalf("migrated pin=%+v, %v", got, err)
	}
}

func TestReviewMaxFixRoundsPinSurvivesFixAndReset(t *testing.T) {
	for _, limit := range []int{0, 2} {
		d := openTestDB(t)
		repo, err := d.InsertRepo(t.TempDir(), "https://example.com/repo", "main")
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
		if err := d.PinReviewMaxFixRounds(step.ID, limit); err != nil {
			t.Fatal(err)
		}
		if err := d.PinReviewMaxFixRounds(step.ID, 9); err != nil {
			t.Fatal(err)
		}
		if err := d.StartStepFixRound(step.ID, 3); err != nil {
			t.Fatal(err)
		}
		if err := d.ResetStepsFrom(run.ID, types.StepReview.Order()); err != nil {
			t.Fatal(err)
		}
		got, err := d.GetStepResult(step.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ReviewMaxFixRounds == nil || *got.ReviewMaxFixRounds != limit {
			t.Fatalf("pinned budget=%v, want %d", got.ReviewMaxFixRounds, limit)
		}
	}
}
