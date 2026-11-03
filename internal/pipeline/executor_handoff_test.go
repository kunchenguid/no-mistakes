package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_HandoffAndPlainAbort(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		status types.RunStatus
		step   types.StepStatus
	}{
		{"handoff", ErrHandoff, types.RunHandedOff, types.StepStatusCompleted},
		{"abort", errors.New(types.RunCancelReasonAbortedByUser), types.RunCancelled, types.StepStatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			const prURL = "https://example.test/o/r/pull/1"
			if err := database.UpdateRunPRURL(run.ID, prURL); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunPRState(run.ID, "open"); err != nil {
				t.Fatal(err)
			}
			workDir := t.TempDir()
			started := make(chan struct{})
			step := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
				close(started)
				<-sctx.Ctx.Done()
				return nil, sctx.Ctx.Err()
			}}
			exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(context.Canceled)
			done := make(chan error, 1)
			go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("CI monitor did not start")
			}
			cancel(tc.cause)
			select {
			case err := <-done:
				if tc.status == types.RunHandedOff && err != nil {
					t.Fatal(err)
				}
				if tc.status == types.RunCancelled && err == nil {
					t.Fatal("plain abort must retain cancellation error")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("CI monitor did not stop")
			}
			got, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.PRURL == nil || *got.PRURL != prURL || got.PRState == nil || *got.PRState != "open" {
				t.Fatalf("PR changed during stop: %+v", got)
			}
			if got.Status != tc.status || !got.Status.Terminal() {
				t.Fatalf("status = %s, want %s terminal", got.Status, tc.status)
			}
			if tc.status == types.RunHandedOff && got.Error != nil {
				t.Fatalf("handoff recorded as error: %s", *got.Error)
			}
			if tc.status == types.RunCancelled && (got.Error == nil || *got.Error != types.RunCancelReasonAbortedByUser) {
				t.Fatalf("plain abort reason = %v", got.Error)
			}
			steps, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(steps) != 1 || steps[0].Status != tc.step {
				t.Fatalf("steps = %+v; want %s", steps, tc.step)
			}
		})
	}
}

func TestExecutor_HandoffRefusesRevokedReview(t *testing.T) {
	database, p, run, repo := setupTest(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	ci := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if err := database.UpdateRunHeadSHAForRevalidation(run.ID, "rewritten-head"); err != nil {
			return nil, err
		}
		cancel(ErrHandoff)
		return nil, sctx.Ctx.Err()
	}}
	executor := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview), ci}, nil)
	if err := executor.Execute(ctx, run, repo, t.TempDir()); err == nil {
		t.Fatal("handed off an unreviewed head")
	}
	recorded, err := database.GetRun(run.ID)
	if err != nil || recorded.Status != types.RunFailed || recorded.ReviewApprovedHeadSHA != nil {
		t.Fatalf("revoked Review completed the run: %+v, %v", recorded, err)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName == types.StepCI && step.Status != types.StepStatusFailed {
			t.Fatalf("CI completed without Review: %+v", step)
		}
	}
}

func TestExecutor_TerminalCompletionRefusesRevokedReview(t *testing.T) {
	for _, status := range []types.RunStatus{types.RunCompleted, types.RunChecksPassed, types.RunHandedOff} {
		t.Run(string(status), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			review, err := database.InsertStepResult(run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.CompleteReviewStep(review.ID, run.ID, run.HeadSHA, 0, 0, ""); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunHeadSHAForRevalidation(run.ID, "rewritten-head"); err != nil {
				t.Fatal(err)
			}
			executor := NewExecutor(database, p, nil, nil, nil, nil)
			if err := executor.completeRunWithStatus(run, repo, status); err == nil {
				t.Fatal("unreviewed head received a success status")
			}
			recorded, err := database.GetRun(run.ID)
			if err != nil || recorded.Status.Terminal() || recorded.ReviewApprovedHeadSHA != nil {
				t.Fatalf("revoked Review completed the run: %+v, %v", recorded, err)
			}
		})
	}
}
