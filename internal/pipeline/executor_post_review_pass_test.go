package pipeline

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	postReviewDocumentHead = "2222222222222222222222222222222222222222"
	postReviewFixedHead    = "3333333333333333333333333333333333333333"
)

// moveHeadStep stands in for a later step that commits: it advances the run's
// recorded head the way commitAgentFixes does.
func moveHeadStep(name types.StepName, head string, calls *int) *adaptiveCallStep {
	return &adaptiveCallStep{name: name, fn: func(sctx *StepContext) (*StepOutcome, error) {
		*calls++
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, head); err != nil {
			return nil, err
		}
		sctx.Run.HeadSHA = head
		return &StepOutcome{}, nil
	}}
}

// pushUntilReviewed stands in for Push under review.post_review_pass: it asks
// for a pass while the head is past the durable approval, and publishes once
// it is not.
func pushUntilReviewed(calls *int) *adaptiveCallStep {
	return &adaptiveCallStep{name: types.StepPush, fn: func(sctx *StepContext) (*StepOutcome, error) {
		*calls++
		run, err := sctx.DB.GetRun(sctx.Run.ID)
		if err != nil {
			return nil, err
		}
		if run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != sctx.Run.HeadSHA {
			return &StepOutcome{PostReviewPass: true}, nil
		}
		return &StepOutcome{}, nil
	}}
}

func roundTriggers(t *testing.T, database *db.DB, stepResultID string) []string {
	t.Helper()
	rounds, err := database.GetRoundsByStep(stepResultID)
	if err != nil {
		t.Fatal(err)
	}
	triggers := make([]string, 0, len(rounds))
	for _, r := range rounds {
		triggers = append(triggers, r.Trigger)
	}
	return triggers
}

func stepRecord(t *testing.T, database *db.DB, runID string, name types.StepName) *db.StepResult {
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
	t.Fatalf("no %s step record", name)
	return nil
}

// A Document commit after Review is reviewed before Push publishes it: the
// review step runs again over exactly the commits after the approved head,
// its finding goes through the ordinary auto-fix round, and the approval then
// advances to the head the pass certified. Test and Document are not re-run.
func TestExecutor_PostReviewPassReviewsLaterCommitsBeforePush(t *testing.T) {
	database, p, run, repo := setupTest(t)
	initialHead := run.HeadSHA
	var reviewCalls, testCalls, documentCalls, pushCalls int
	var passBases []string
	review := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		reviewCalls++
		passBases = append(passBases, sctx.PostReviewPassFrom)
		switch reviewCalls {
		case 1:
			return &StepOutcome{ReviewApprovedHeadSHA: sctx.Run.HeadSHA}, nil
		case 2:
			if sctx.Fixing {
				return nil, fmt.Errorf("the pass's first turn is a review, not a fix round")
			}
			return &StepOutcome{
				AutoFixable:           true,
				NeedsApproval:         true,
				Findings:              `{"findings":[{"id":"r1","severity":"error","file":"README.md","line":3,"description":"documents a flag the code does not have","action":"auto-fix"}]}`,
				ReviewApprovedHeadSHA: sctx.Run.HeadSHA,
			}, nil
		default:
			if !sctx.Fixing {
				return nil, fmt.Errorf("expected the pass's auto-fix round")
			}
			if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, postReviewFixedHead); err != nil {
				return nil, err
			}
			sctx.Run.HeadSHA = postReviewFixedHead
			return &StepOutcome{ReviewedPaths: []string{"README.md"}, ReviewablePaths: []string{"README.md"}, ReviewApprovedHeadSHA: postReviewFixedHead}, nil
		}
	}}
	test := &adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
		testCalls++
		return &StepOutcome{}, nil
	}}
	steps := []Step{review, test, moveHeadStep(types.StepDocument, postReviewDocumentHead, &documentCalls), pushUntilReviewed(&pushCalls)}
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Review: 1}}, nil, steps, nil)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}

	if reviewCalls != 3 || testCalls != 1 || documentCalls != 1 || pushCalls != 2 {
		t.Fatalf("calls review=%d test=%d document=%d push=%d, want 3/1/1/2 (Review alone runs again)", reviewCalls, testCalls, documentCalls, pushCalls)
	}
	if want := []string{"", initialHead, initialHead}; fmt.Sprint(passBases) != fmt.Sprint(want) {
		t.Fatalf("review bases = %q, want %q: the pass reviews from the head Review approved", passBases, want)
	}
	reviewRecord := stepRecord(t, database, run.ID, types.StepReview)
	if got, want := roundTriggers(t, database, reviewRecord.ID), []string{"initial", db.RoundTriggerPostReview, "auto_fix"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("review round triggers = %q, want %q", got, want)
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewApprovedHeadSHA == nil || *got.ReviewApprovedHeadSHA != postReviewFixedHead {
		t.Fatalf("review-approved head = %#v, want the head the pass certified %s", got.ReviewApprovedHeadSHA, postReviewFixedHead)
	}
	if got.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed", got.Status)
	}
	for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepPush} {
		if s := stepRecord(t, database, run.ID, name); s.Status != types.StepStatusCompleted {
			t.Fatalf("%s status = %s, want completed", name, s.Status)
		}
	}
}

// A daemon restart while the pass is parked resumes the pass - not the Test
// step after Review - and the fix round still reviews from the approved head.
func TestExecutor_RecoveredPostReviewPassResumesAtPush(t *testing.T) {
	database, p, run, repo := setupTest(t)
	approvedHead := run.HeadSHA
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, approvedHead); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, postReviewDocumentHead); err != nil {
		t.Fatal(err)
	}
	records := map[types.StepName]*db.StepResult{}
	for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepPush} {
		sr, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		records[name] = sr
	}
	for _, name := range []types.StepName{types.StepTest, types.StepDocument} {
		if err := database.CompleteStepWithStatus(records[name].ID, types.StepStatusCompleted, 0, 10, ""); err != nil {
			t.Fatal(err)
		}
	}
	reviewID := records[types.StepReview].ID
	if err := database.StartStep(reviewID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"r1","severity":"error","file":"README.md","line":3,"description":"documents a flag the code does not have","action":"auto-fix"}]}`
	if err := database.SetStepFindings(reviewID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(reviewID, 1, "initial", nil, nil, approvedHead, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(reviewID, 2, db.RoundTriggerPostReview, &findings, nil, postReviewDocumentHead, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(reviewID, types.StepStatusAwaitingApproval, 20); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	var testCalls, documentCalls, pushCalls int
	var fixBase string
	review := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return nil, fmt.Errorf("expected the recovered fix round")
		}
		fixBase = sctx.PostReviewPassFrom
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, postReviewFixedHead); err != nil {
			return nil, err
		}
		sctx.Run.HeadSHA = postReviewFixedHead
		return &StepOutcome{ReviewedPaths: []string{"README.md"}, ReviewablePaths: []string{"README.md"}, ReviewApprovedHeadSHA: postReviewFixedHead}, nil
	}}
	test := &adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
		testCalls++
		return &StepOutcome{}, nil
	}}
	steps := []Step{review, test, moveHeadStep(types.StepDocument, postReviewDocumentHead, &documentCalls), pushUntilReviewed(&pushCalls)}
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Review: 1}}, nil, steps, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()
	deadline := time.Now().Add(5 * time.Second)
	for exec.Respond(types.StepReview, types.ActionFix, []string{"r1"}) != nil {
		if time.Now().After(deadline) {
			t.Fatal("recovered post-review gate never accepted a response")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("recovered executor timed out")
	}

	if fixBase != approvedHead {
		t.Fatalf("recovered fix round reviewed from %q, want the approved head %s", fixBase, approvedHead)
	}
	if testCalls != 0 || documentCalls != 0 || pushCalls != 1 {
		t.Fatalf("calls test=%d document=%d push=%d, want 0/0/1: recovery returns to Push", testCalls, documentCalls, pushCalls)
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewApprovedHeadSHA == nil || *got.ReviewApprovedHeadSHA != postReviewFixedHead {
		t.Fatalf("review-approved head = %#v, want %s", got.ReviewApprovedHeadSHA, postReviewFixedHead)
	}
	if got.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed", got.Status)
	}
}

// Only a review gate may sit before completed steps; anything else after a gate
// is still an unrecoverable plan.
func TestExecutor_RecoveredGateRefusesCompletedStepsAfterANonReviewGate(t *testing.T) {
	database, _, run, _ := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	test, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	document, err := database.InsertStepResult(run.ID, types.StepDocument)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepResult(run.ID, types.StepPush); err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(test.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"t1","severity":"error","description":"failing","action":"ask-user"}]}`
	if err := database.SetStepFindings(test.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(test.ID, 1, "initial", &findings, nil, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(test.ID, types.StepStatusAwaitingApproval, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(document.ID, types.StepStatusCompleted, 0, 10, ""); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	steps := []Step{&mockStep{name: types.StepTest}, &mockStep{name: types.StepDocument}, &mockStep{name: types.StepPush}}
	if err := ValidateRecoveredRun(database, run, steps); err == nil {
		t.Fatal("a completed step after a test gate was accepted as recoverable")
	}
}
