package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestExecutor_HumanReviewFindingsRequireApprovalWithoutNeedsApprovalFlag proves
// an ask-user finding gates the step for approval even when the step did not set
// NeedsApproval. (The legacy numeric pre-gate auto-fix loop was removed with the
// routing cutover; repair is owned by the coordinator, so only the gate logic
// remains under test here.)
func TestExecutor_HumanReviewFindingsRequireApprovalWithoutNeedsApprovalFlag(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			return &StepOutcome{
				NeedsApproval: false,
				AutoFixable:   true,
				Findings:      `{"findings":[{"severity":"info","description":"design choice","action":"ask-user"}],"summary":"1 issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	exec.Respond(types.StepReview, types.ActionApprove, nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}
