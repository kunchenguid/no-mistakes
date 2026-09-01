package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const errorAutoFixFinding = `{"findings":[{"id":"review-1","severity":"error","description":"bug","action":"auto-fix"}],"summary":"1 issue"}`

// review.max_fix_rounds bounds the automatic loop even when auto_fix.review
// would allow more: after the cap the step parks, a further fix is refused,
// and approve still completes the run.
func TestExecutor_MaxFixRoundsCapsAutomaticFixRounds(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 3}, Review: config.Review{MaxFixRounds: 1}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			// Never converges: every round reports the same blocker.
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: errorAutoFixFinding}, nil
		},
	}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	if callCount != 2 {
		t.Fatalf("step calls = %d, want 2 (initial + the one permitted fix round)", callCount)
	}

	err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"})
	var exhausted *FixRoundsExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("fix after the cap: err = %v, want *FixRoundsExhaustedError", err)
	}
	if exhausted.Used != 1 || exhausted.Max != 1 || exhausted.Step != types.StepReview {
		t.Fatalf("exhausted = %+v, want used 1 of max 1 on review", exhausted)
	}
	// The gate is still open after the refusal.
	if got, _ := database.GetRun(run.ID); got.Status != types.RunRunning {
		t.Fatalf("run status after refused fix = %q, want still running", got.Status)
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve after the cap: %v", err)
	}
	waitExecutorDone(t, done)
	if callCount != 2 {
		t.Fatalf("step calls = %d after approve, want no further round", callCount)
	}
	if got, _ := database.GetRun(run.ID); got.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want completed", got.Status)
	}
}

// Gate-driven fixes count against the same cap as automatic ones: this is the
// unbounded `axi respond --action fix` ratchet the cap exists to end.
func TestExecutor_MaxFixRoundsCountsGateDrivenFixRounds(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 0}, Review: config.Review{MaxFixRounds: 1}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: errorAutoFixFinding}, nil
		},
	}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	// auto_fix.review is 0, so the initial round parks; the first gate-driven fix is allowed.
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatalf("first gate-driven fix: %v", err)
	}
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	if callCount != 2 {
		t.Fatalf("step calls = %d, want 2", callCount)
	}
	var exhausted *FixRoundsExhaustedError
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); !errors.As(err, &exhausted) {
		t.Fatalf("second gate-driven fix: err = %v, want the cap to refuse it", err)
	}
	if err := exec.Respond(types.StepReview, types.ActionSkip, nil); err != nil {
		t.Fatalf("skip after the cap: %v", err)
	}
	waitExecutorDone(t, done)
}

// Without a cap the gate-driven loop stays unbounded, exactly as before.
func TestExecutor_NoMaxFixRoundsLeavesGateDrivenFixesUnbounded(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	cfg := &config.Config{}

	callCount := 0
	called := make(chan int, 8)
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			called <- callCount
			if callCount >= 4 {
				return &StepOutcome{}, nil
			}
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: errorAutoFixFinding}, nil
		},
	}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)
	<-called
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	for i := 0; i < 3; i++ {
		if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
			t.Fatalf("gate-driven fix %d: %v", i+1, err)
		}
		// Wait for the fix round to actually run before polling for its park;
		// the DB still reads fix_review from the previous park until then.
		<-called
		if i < 2 {
			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
		}
	}
	waitExecutorDone(t, done)
	if callCount != 4 {
		t.Fatalf("step calls = %d, want 4", callCount)
	}
}

// review.auto_fix_ask_user sends ask-user findings through the automatic fix
// loop with the auto-fix ones instead of parking for each.
func TestExecutor_AutoFixAskUserFixesAskUserFindingsWithoutParking(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 2}, Review: config.Review{AutoFixAskUser: true}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					AutoFixable:   true,
					Findings: `{"findings":[
						{"id":"review-1","severity":"error","description":"bug","action":"auto-fix"},
						{"id":"review-2","severity":"error","description":"intent question","action":"ask-user"}
					],"summary":"2 issues"}`,
				}, nil
			}
			parsed, _ := types.ParseFindingsJSON(sctx.PreviousFindings)
			if len(parsed.Items) != 2 {
				t.Errorf("fix round received %d findings, want both the auto-fix and the ask-user one", len(parsed.Items))
			}
			return &StepOutcome{}, nil
		},
	}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("step calls = %d, want 2 (initial + one fix round, no park)", callCount)
	}
	if got, _ := database.GetRun(run.ID); got.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want completed without a gate", got.Status)
	}
}

// Under review.gate_severity: error a warning-severity ask-user finding is
// reported but never parks the step.
func TestExecutor_GateSeverityErrorDoesNotParkOnWarningAskUser(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	cfg := &config.Config{Review: config.Review{GateSeverity: config.ReviewGateSeverityError}}

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			return &StepOutcome{
				NeedsApproval: false, // the review step computed this under gate_severity: error
				AutoFixable:   true,
				Findings:      `{"findings":[{"id":"review-1","severity":"warning","description":"nit","action":"ask-user"}],"summary":"1 warning"}`,
			}, nil
		},
	}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got, _ := database.GetRun(run.ID); got.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want completed without parking on a warning", got.Status)
	}
}

// ...but an error finding parks even if the step forgot to set NeedsApproval.
func TestExecutor_GateSeverityErrorStillParksOnErrorFinding(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	cfg := &config.Config{Review: config.Review{GateSeverity: config.ReviewGateSeverityError}}

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			return &StepOutcome{
				NeedsApproval: false,
				AutoFixable:   true,
				Findings:      `{"findings":[{"id":"review-1","severity":"error","description":"bug","action":"ask-user"}],"summary":"1 error"}`,
			}, nil
		},
	}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitExecutorDone(t, done)
}
