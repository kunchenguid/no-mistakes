package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// StartStepWithLimits records the review fix-round cap beside the auto-fix
// limit so read-only surfaces can render the budget; 0 is stored as NULL.
func TestStartStepWithLimits_RecordsMaxFixRounds(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/repo.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	capped, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if err := d.StartStepWithLimits(capped.ID, 2, 3); err != nil {
		t.Fatalf("start step with limits: %v", err)
	}
	got, err := d.GetStepResult(capped.ID)
	if err != nil {
		t.Fatalf("get step: %v", err)
	}
	if got.AutoFixLimit == nil || *got.AutoFixLimit != 2 || got.MaxFixRounds == nil || *got.MaxFixRounds != 3 {
		t.Fatalf("limits = auto %v / max %v, want 2 / 3", got.AutoFixLimit, got.MaxFixRounds)
	}
	if got.Status != types.StepStatusRunning {
		t.Fatalf("status = %q, want running", got.Status)
	}

	unbounded, err := d.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if err := d.StartStepWithLimits(unbounded.ID, 1, 0); err != nil {
		t.Fatalf("start step: %v", err)
	}
	got, err = d.GetStepResult(unbounded.ID)
	if err != nil {
		t.Fatalf("get step: %v", err)
	}
	if got.MaxFixRounds != nil {
		t.Fatalf("max_fix_rounds = %d, want NULL for an unbounded step", *got.MaxFixRounds)
	}
}
