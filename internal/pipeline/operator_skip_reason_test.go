package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const skipReason = "docs-only change, nothing to review"

func stepNamed(t *testing.T, database *db.DB, runID string, name types.StepName) *db.StepResult {
	t.Helper()
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.StepName == name {
			return s
		}
	}
	t.Fatalf("run %s has no %s step", runID, name)
	return nil
}

// A skip answered at a gate is the operator's decision and keeps their reason,
// apart from the automatic skip cause.
func TestExecutor_SkipAtGateRecordsTheOperatorsReason(t *testing.T) {
	exec, database, run, done, _, testRan := parkReviewGate(t)

	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionSkip, nil, nil, nil, nil, skipReason); err != nil {
		t.Fatalf("RespondWithOverrides(skip) = %v", err)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	review := stepNamed(t, database, run.ID, types.StepReview)
	if reason, ok := review.OperatorSkip(); !ok || reason != skipReason {
		t.Fatalf("OperatorSkip() = %q, %v; want the operator's reason", reason, ok)
	}
	if review.Status != types.StepStatusSkipped || review.SkipReason != nil {
		t.Fatalf("review = %s / skip_reason %v, want an operator skip with no automatic cause", review.Status, review.SkipReason)
	}
	if testRan.Load() != 1 {
		t.Fatalf("the step after a skipped gate ran %d times, want 1", testRan.Load())
	}
}

// A skip sent without a reason (the TUI's skip key) is still the operator's
// decision, read with the fallback reason.
func TestExecutor_SkipAtGateWithoutAReasonIsStillAnOperatorSkip(t *testing.T) {
	exec, database, run, done, _, _ := parkReviewGate(t)
	if err := exec.Respond(types.StepReview, types.ActionSkip, nil); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if reason, ok := stepNamed(t, database, run.ID, types.StepReview).OperatorSkip(); !ok || reason != db.NoOperatorReasonSupplied {
		t.Fatalf("OperatorSkip() = %q, %v; want the fallback reason", reason, ok)
	}
}

// A skip answered at a gate recovered after a daemon restart records the
// reason the same way.
func TestExecutor_RecoveredGateSkipRecordsTheOperatorsReason(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(stepResult.ID, declinedTestFindings); err != nil {
		t.Fatal(err)
	}
	findings := declinedTestFindings
	if _, err := database.InsertStepRound(stepResult.ID, 1, "initial", &findings, nil, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(database, p, nil, nil, []Step{newApprovalStep(types.StepTest, declinedTestFindings)}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := exec.RespondWithOverrides(types.StepTest, types.ActionSkip, nil, nil, nil, nil, skipReason); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovered gate never accepted the skip")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := waitDone(t, done); err != nil {
		t.Fatalf("Resume() = %v", err)
	}
	if reason, ok := stepNamed(t, database, run.ID, types.StepTest).OperatorSkip(); !ok || reason != skipReason {
		t.Fatalf("OperatorSkip() = %q, %v; want the operator's reason", reason, ok)
	}
}

// Steps skipped at run start record the reason the operator gave for the run's
// skips; steps the run executed are not operator skips.
func TestExecutor_RequestedSkipsRecordTheOperatorsReason(t *testing.T) {
	database, p, run, repo := setupTest(t)
	review, test := newPassStep(types.StepReview), newPassStep(types.StepTest)
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{review, test}, nil)
	exec.SetSkippedSteps([]types.StepName{types.StepReview}, skipReason)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if review.callCount() != 0 {
		t.Fatalf("a requested skip executed %d times", review.callCount())
	}
	if reason, ok := stepNamed(t, database, run.ID, types.StepReview).OperatorSkip(); !ok || reason != skipReason {
		t.Fatalf("review OperatorSkip() = %q, %v; want the run's skip reason", reason, ok)
	}
	if _, ok := stepNamed(t, database, run.ID, types.StepTest).OperatorSkip(); ok {
		t.Fatal("an executed step reads as an operator skip")
	}
}

// A retry carries the source run's operator skips with their reasons, and a
// source skip that recorded none stays an ordinary skip.
func TestExecutor_RetryCarriesOperatorSkipReasons(t *testing.T) {
	database, p, _, repo := setupTest(t)
	names := []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint}
	source := failedSourceRun(t, database, repo, names, types.StepTest, types.StepDocument, types.StepLint)
	if err := database.CompleteOperatorSkippedStep(stepNamed(t, database, source.ID, types.StepDocument).ID, 0, 0, "", skipReason); err != nil {
		t.Fatal(err)
	}
	continuation, err := database.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(database, p, &config.Config{}, nil, stepsNamed(names...), nil)
	exec.SetRetrySource(source.ID)
	if err := exec.Execute(context.Background(), continuation, repo, t.TempDir()); err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if reason, ok := stepNamed(t, database, continuation.ID, types.StepDocument).OperatorSkip(); !ok || reason != skipReason {
		t.Fatalf("document OperatorSkip() = %q, %v; want the source run's reason", reason, ok)
	}
	lint := stepNamed(t, database, continuation.ID, types.StepLint)
	if _, ok := lint.OperatorSkip(); ok || lint.Status != types.StepStatusSkipped {
		t.Fatalf("lint = %s / operator skip %v, want an ordinary skip", lint.Status, ok)
	}
}
