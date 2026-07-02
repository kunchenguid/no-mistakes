package daemon

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestStepToInfoIncludesFixSummaries(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc", "def")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview, 0)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}

	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"x"}],"summary":"1"}`
	if _, err := d.InsertStepRound(step.ID, 1, "initial", &findings, nil, 100); err != nil {
		t.Fatalf("insert round 1: %v", err)
	}
	sum := "handle nil pointer in executor"
	if _, err := d.InsertStepRound(step.ID, 2, "auto_fix", nil, &sum, 100); err != nil {
		t.Fatalf("insert round 2: %v", err)
	}

	info := stepToInfo(d, step)
	if len(info.FixSummaries) != 1 || info.FixSummaries[0] != sum {
		t.Errorf("fix summaries = %v, want [%q]", info.FixSummaries, sum)
	}
}

func TestStepToInfoNoFixSummariesWithoutFixRounds(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc", "def")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepLint, 0)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if _, err := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 100); err != nil {
		t.Fatalf("insert round: %v", err)
	}

	info := stepToInfo(d, step)
	if len(info.FixSummaries) != 0 {
		t.Errorf("fix summaries = %v, want none", info.FixSummaries)
	}
}
