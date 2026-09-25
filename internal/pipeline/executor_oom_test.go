package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_OutOfMemoryFailureReasonKeepsRestorationDetail(t *testing.T) {
	database, p, run, repo := setupTest(t)
	const snapshot = "/tmp/nm-recovery-snapshot"
	stepErr := fmt.Errorf("restore worktree failed; retained snapshot %s: %w", snapshot, shellenv.ErrOutOfMemory)

	exec := NewExecutor(database, p, nil, nil, []Step{newFailStep(types.StepTest, stepErr)}, nil)
	err := exec.Execute(context.Background(), run, repo, t.TempDir())
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	dbSteps, _ := database.GetStepsByRun(run.ID)
	if dbSteps[0].Error == nil {
		t.Fatal("failed step has no recorded reason")
	}
	reason := *dbSteps[0].Error
	for _, want := range []string{"restore worktree failed", snapshot, shellenv.ErrOutOfMemory.Error()} {
		if !strings.Contains(reason, want) {
			t.Fatalf("step failure reason %q is missing %q", reason, want)
		}
	}
}
