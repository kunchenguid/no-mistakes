package pipeline

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_BoundedReviewAdjudicatesManyFindingsInOneCorrectionThenContinues(t *testing.T) {
	database, paths, run, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	headBytes, err := exec.Command("git", "-C", workDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = strings.TrimSpace(string(headBytes))
	run.BaseSHA = run.HeadSHA
	if err := database.UpdateRunHeadSHA(run.ID, run.HeadSHA); err != nil {
		t.Fatal(err)
	}

	var correctionCalls int
	var selected, deferred types.Findings
	review := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: boundedReviewFindingsFixture, ReviewApprovedHeadSHA: run.HeadSHA}, nil
		}
		correctionCalls++
		var err error
		selected, err = types.ParseFindingsJSON(sctx.PreviousFindings)
		if err != nil {
			t.Fatalf("parse correction selection: %v", err)
		}
		deferred, err = types.ParseFindingsJSON(sctx.DeferredFindings)
		if err != nil {
			t.Fatalf("parse deferred findings: %v", err)
		}
		return &StepOutcome{Findings: mergeFindingsJSON(sctx.PreviousFindings, sctx.DeferredFindings), ReviewApprovedHeadSHA: run.HeadSHA}, nil
	}}
	verification := newPassStep(types.StepTest)
	push := newPassStep(types.StepPush)
	pr := newPassStep(types.StepPR)
	ci := newPassStep(types.StepCI)
	cfg := &config.Config{
		Review:  config.Review{Strategy: config.ReviewStrategyBounded},
		AutoFix: config.AutoFix{Review: 9}, // Bounded mode must ignore automatic review loops.
	}
	exec := NewExecutor(database, paths, cfg, nil, []Step{review, verification, push, pr, ci}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	dispositions := map[string]types.FindingDisposition{
		"review-1": {Decision: types.FindingDispositionFix, Reason: "reproduced"},
		"review-2": {Decision: types.FindingDispositionReject, Reason: "caller excludes the case"},
		"review-3": {Decision: types.FindingDispositionDefer, Reason: "API redesign is outside scope"},
	}
	if err := exec.RespondWithAdjudication(types.StepReview, types.ActionFix, []string{"review-1"}, nil, nil, dispositions, ""); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bounded review pipeline did not complete")
	}
	if correctionCalls != 1 {
		t.Fatalf("correction calls = %d, want exactly one", correctionCalls)
	}
	if len(selected.Items) != 1 || selected.Items[0].ID != "review-1" || selected.Items[0].Disposition != types.FindingDispositionFix {
		t.Fatalf("correction received %+v, want only confirmed review-1", selected.Items)
	}
	if len(deferred.Items) != 2 || deferred.Items[0].Disposition != types.FindingDispositionReject || deferred.Items[1].Disposition != types.FindingDispositionDefer {
		t.Fatalf("non-mutating findings = %+v", deferred.Items)
	}
	for name, calls := range map[string]int{
		"test": verification.callCount(), "push": push.callCount(), "pr": pr.callCount(), "ci": ci.callCount(),
	} {
		if calls != 1 {
			t.Fatalf("%s calls = %d, want one after bounded correction", name, calls)
		}
	}
}

func TestExecutor_BoundedReviewRejectsIncompleteOrMismatchedAdjudication(t *testing.T) {
	database, paths, run, repo := setupTest(t)
	workDir := t.TempDir()
	review := newApprovalStep(types.StepReview, boundedReviewFindingsFixture)
	exec := NewExecutor(database, paths, &config.Config{Review: config.Review{Strategy: config.ReviewStrategyBounded}}, nil, []Step{review}, nil)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	incomplete := map[string]types.FindingDisposition{
		"review-1": {Decision: types.FindingDispositionFix, Reason: "reproduced"},
	}
	if err := exec.RespondWithAdjudication(types.StepReview, types.ActionFix, []string{"review-1"}, nil, nil, incomplete, ""); err == nil {
		t.Fatal("incomplete dispositions were accepted")
	}
	complete := map[string]types.FindingDisposition{
		"review-1": {Decision: types.FindingDispositionFix, Reason: "reproduced"},
		"review-2": {Decision: types.FindingDispositionReject, Reason: "not reachable"},
		"review-3": {Decision: types.FindingDispositionDefer, Reason: "outside scope"},
	}
	if err := exec.RespondWithAdjudication(types.StepReview, types.ActionFix, []string{"review-1", "review-2"}, nil, nil, complete, ""); err == nil {
		t.Fatal("finding selection that included a rejected finding was accepted")
	}
	if err := exec.RespondWithAdjudication(types.StepReview, types.ActionFix, []string{"review-1"}, nil, []types.Finding{{ID: "worker-added", Description: "extra scope"}}, complete, ""); err == nil {
		t.Fatal("worker-added finding bypassed the bounded reviewer report")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not stop after cancellation")
	}
}

func TestExecutor_BoundedReviewRecoveryRetainsDispositionedCorrection(t *testing.T) {
	database, paths, run, repo := setupTest(t)
	stepResult, recoveredRun := seedRecoveredReviewGate(t, database, run, boundedReviewFindingsFixture, types.StepStatusAwaitingApproval, "")
	started := make(chan struct{})
	release := make(chan struct{})
	var selected, deferred types.Findings
	review := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		var err error
		selected, err = types.ParseFindingsJSON(sctx.PreviousFindings)
		if err != nil {
			return nil, err
		}
		deferred, err = types.ParseFindingsJSON(sctx.DeferredFindings)
		if err != nil {
			return nil, err
		}
		close(started)
		<-release
		return &StepOutcome{Findings: mergeFindingsJSON(sctx.PreviousFindings, sctx.DeferredFindings), ReviewApprovedHeadSHA: recoveredRun.HeadSHA}, nil
	}}
	exec := NewExecutor(database, paths, &config.Config{Review: config.Review{Strategy: config.ReviewStrategyBounded}}, nil, []Step{review}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	workDir := t.TempDir()
	go func() { done <- exec.Resume(ctx, recoveredRun, repo, workDir) }()

	dispositions := map[string]types.FindingDisposition{
		"review-1": {Decision: types.FindingDispositionFix, Reason: "reproduced"},
		"review-2": {Decision: types.FindingDispositionReject, Reason: "not reachable"},
		"review-3": {Decision: types.FindingDispositionDefer, Reason: "outside scope"},
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := exec.RespondWithAdjudication(types.StepReview, types.ActionFix, []string{"review-1"}, nil, nil, dispositions, "")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovered bounded gate never accepted dispositions: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("recovered bounded correction did not start")
	}
	if len(selected.Items) != 1 || selected.Items[0].ID != "review-1" || len(deferred.Items) != 2 {
		t.Fatalf("recovered selection=%+v deferred=%+v", selected.Items, deferred.Items)
	}
	rounds, err := database.GetRoundsByStep(stepResult.ID)
	if err != nil || len(rounds) != 1 || rounds[0].UserFindingsJSON == nil {
		t.Fatalf("recovered decision evidence missing: rounds=%+v err=%v", rounds, err)
	}
	persisted, err := types.ParseFindingsJSON(*rounds[0].UserFindingsJSON)
	if err != nil || len(persisted.Items) != 3 || persisted.Items[1].Disposition != types.FindingDispositionReject {
		t.Fatalf("persisted dispositions=%+v err=%v", persisted.Items, err)
	}

	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recovered executor did not stop")
	}
}
