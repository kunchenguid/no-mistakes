package pipeline

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutorResolvesPRTargetBeforeRebaseAndCarriesItToReview(t *testing.T) {
	database, p, run, repo := setupTest(t)
	var observed []types.StepName
	steps := []Step{
		newPassStep(types.StepIntent),
		&adaptiveCallStep{name: types.StepRebase, fn: func(sctx *StepContext) (*StepOutcome, error) {
			if sctx.PRTarget == nil || sctx.PRTarget.TargetBranch != "develop" {
				t.Fatalf("rebase target = %+v, want develop", sctx.PRTarget)
			}
			return &StepOutcome{}, nil
		}},
		&adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
			if sctx.PRTarget == nil || sctx.PRTarget.TargetBranch != "develop" {
				t.Fatalf("review target = %+v, want develop", sctx.PRTarget)
			}
			return &StepOutcome{}, nil
		}},
	}
	executor := NewExecutor(database, p, nil, nil, steps, nil)
	executor.SetPRContextGuard(func(_ *StepContext, name types.StepName) (PRContextDecision, error) {
		observed = append(observed, name)
		return PRContextDecision{Target: PRTargetSelection{TargetBranch: "develop"}}, nil
	})
	if err := executor.Execute(context.Background(), run, repo, completionFixture(t, database, run)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(observed, []types.StepName{types.StepRebase, types.StepRebase, types.StepReview, types.StepReview}) {
		t.Fatalf("guard called at %v", observed)
	}
}

func TestExecutorParkedApprovalRevalidatesPRTargetBeforeFinalStepCompletes(t *testing.T) {
	database, p, run, repo := setupTest(t)
	var observed []types.StepName
	reviewRuns := 0
	steps := []Step{
		newPassStep(types.StepIntent),
		&adaptiveCallStep{name: types.StepRebase, fn: func(*StepContext) (*StepOutcome, error) {
			observed = append(observed, types.StepRebase)
			return &StepOutcome{}, nil
		}},
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			observed = append(observed, types.StepReview)
			reviewRuns++
			if reviewRuns == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"severity":"warning","description":"old comparison","action":"ask-user"}]}`}, nil
			}
			return &StepOutcome{ReviewApprovedHeadSHA: "2222222222222222222222222222222222222222"}, nil
		}},
	}
	executor := NewExecutor(database, p, nil, nil, steps, nil)
	checks := 0
	executor.SetPRContextGuard(func(_ *StepContext, name types.StepName) (PRContextDecision, error) {
		checks++
		if checks == 5 {
			if err := database.ResetStepsFrom(run.ID, types.StepRebase.Order()); err != nil {
				return PRContextDecision{}, err
			}
			return PRContextDecision{Target: PRTargetSelection{TargetBranch: "develop"}, RestartFrom: types.StepRebase}, nil
		}
		return PRContextDecision{Target: PRTargetSelection{TargetBranch: "develop"}}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	workDir := completionFixture(t, database, run)
	go func() { done <- executor.Execute(ctx, run, repo, workDir) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := executor.Respond(types.StepReview, types.ActionApprove, nil); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("review never parked for approval")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(observed, []types.StepName{types.StepRebase, types.StepReview, types.StepRebase, types.StepReview}) {
		t.Fatalf("executed steps = %v, want fresh rebase and review", observed)
	}
}

func TestExecutorRechecksTerminalCIProof(t *testing.T) {
	database, p, run, repo := setupTest(t)
	const head = "1111111111111111111111111111111111111111"
	if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/acme/repo/pull/7"
	_, err := database.BindRunPRContext(run.ID, db.PRContextCandidate{
		PRURL: prURL, SourceRepo: "acme/repo", SourceBranch: run.Branch,
		ForgeHeadSHA: head, LocalHeadSHA: head, TargetBranch: "develop",
		TargetSHA:    "2222222222222222222222222222222222222222",
		MergeBaseSHA: "2222222222222222222222222222222222222222",
		DiffDigest:   "3333333333333333333333333333333333333333333333333333333333333333",
	}, types.StepRebase)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRState(run.ID, "merged"); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepCI)}, nil)
	called := false
	executor.SetPRContextGuard(func(sctx *StepContext, step types.StepName) (PRContextDecision, error) {
		called = true
		if step != types.StepCI || !sctx.PRContextAfterStep {
			t.Fatalf("terminal guard context = %s, after=%t", step, sctx.PRContextAfterStep)
		}
		return PRContextDecision{Target: PRTargetSelection{PRURL: prURL, TargetBranch: "develop"}}, nil
	})
	index, err := executor.checkPRContext(context.Background(), run, repo, t.TempDir(), types.StepCI, 0, true)
	if err != nil || index != -1 || !called {
		t.Fatalf("terminal CI context = %d, %v", index, err)
	}
}

func TestExecutorReviewPendingTestClaimAdvancesToOwnerStep(t *testing.T) {
	database, p, run, repo := setupTest(t)
	testRan := false
	const pending = `{"findings":[{"severity":"warning","description":"old test output needs current proof","action":"no-op","category":"review-support-pending","support":{"claim_type":"test","test":{"command":"go test ./..."}}}]}`
	executor := NewExecutor(database, p, nil, nil, []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{Findings: pending, ReviewApprovedHeadSHA: "1111111111111111111111111111111111111111"}, nil
		}},
		&adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
			testRan = true
			return &StepOutcome{}, nil
		}},
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := executor.Execute(ctx, run, repo, t.TempDir()); err == nil {
		t.Fatal("pending claim completed without current owner proof")
	}
	if !testRan {
		t.Fatal("pending Test claim never reached Test")
	}
}

func TestExecutorSkippedEvidenceOwnerCannotCompletePendingClaim(t *testing.T) {
	database, p, run, repo := setupTest(t)
	const pending = `{"findings":[{"severity":"warning","description":"old test output needs current proof","action":"no-op","category":"review-support-pending","support":{"claim_type":"test","test":{"command":"go test ./..."}}}]}`
	executor := NewExecutor(database, p, nil, nil, []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{Findings: pending, ReviewApprovedHeadSHA: "1111111111111111111111111111111111111111"}, nil
		}},
		newPassStep(types.StepTest),
	}, nil)
	executor.SetSkippedSteps([]types.StepName{types.StepTest})
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err == nil {
		t.Fatal("skipped Test owner completed a run with a pending test claim")
	}
}

func TestExecutorCurrentOwnerProofCompletesPendingTestClaim(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)
	head := run.HeadSHA
	const target = "2222222222222222222222222222222222222222"
	const digest = "3333333333333333333333333333333333333333333333333333333333333333"
	if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	_, err := database.BindRunPRContext(run.ID, db.PRContextCandidate{
		LocalHeadSHA: head, TargetBranch: "develop", TargetSHA: target,
		MergeBaseSHA: target, DiffDigest: digest,
	}, types.StepRebase)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := database.GetRunPRContext(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	claim := types.Finding{
		ID: "review-claim-1", Severity: types.FindingSeverityWarning,
		Description: "historical test failure", Action: types.ActionNoOp,
		Category: "review-support-pending", Support: &types.FindingSupport{
			ClaimType: types.FindingClaimTest, Test: &types.FindingTestSupport{Command: "go test ./..."},
		},
	}
	claimJSON, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{claim}})
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	resolved := claim
	resolved.ID = types.ReviewSupportClaimID(claim)
	resolved.Severity = types.FindingSeverityInfo
	resolved.Category = types.FindingCategoryReviewSupportResolved
	resolved.Support = &types.FindingSupport{
		ClaimType: types.FindingClaimTest, Test: &types.FindingTestSupport{Command: "go test ./..."},
		OwnerResult: &types.FindingOwnerResult{
			ReviewFindingID: claim.ID, HeadSHA: head, TargetSHA: target,
			DiffDigest: digest, Generation: receipt.Generation,
			ObservedAt:  time.Now().UTC().Format(time.RFC3339Nano),
			Disposition: types.FindingSupportDispositionDisproven, ExitCode: &zero,
		},
	}
	resolvedJSON, err := json.Marshal(types.Findings{Items: []types.Finding{resolved}})
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(database, p, nil, nil, []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{Findings: claimJSON, ReviewApprovedHeadSHA: head}, nil
		}},
		&adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{Findings: string(resolvedJSON)}, nil
		}},
	}, nil)
	if err := executor.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatal(err)
	}
}

func TestExecutorTestRestartPrecedesFinalSupportValidation(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)
	const pending = `{"findings":[{"id":"review-claim-1","severity":"warning","description":"old test result needs current proof","action":"no-op","category":"review-support-pending","support":{"claim_type":"test","test":{"command":"go test ./..."}}}]}`
	reviewCalls, testCalls := 0, 0
	executor := NewExecutor(database, p, nil, nil, []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			reviewCalls++
			if reviewCalls == 1 {
				return &StepOutcome{Findings: pending, ReviewApprovedHeadSHA: run.HeadSHA}, nil
			}
			return &StepOutcome{ReviewApprovedHeadSHA: run.HeadSHA}, nil
		}},
		&adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
			testCalls++
			if testCalls == 1 {
				return &StepOutcome{RestartFrom: types.StepReview}, nil
			}
			return &StepOutcome{}, nil
		}},
	}, nil)
	if err := executor.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatal(err)
	}
	if reviewCalls != 2 || testCalls != 2 {
		t.Fatalf("restart calls: review=%d test=%d", reviewCalls, testCalls)
	}
}

func TestExecutorUnresolvedTestClaimStopsBeforePublication(t *testing.T) {
	database, p, run, repo := setupTest(t)
	const pending = `{"findings":[{"id":"review-claim-1","severity":"warning","description":"old test output needs current proof","action":"no-op","category":"review-support-pending","support":{"claim_type":"test","test":{"command":"go test ./..."}}}]}`
	pushRan := false
	executor := NewExecutor(database, p, nil, nil, []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{Findings: pending, ReviewApprovedHeadSHA: "1111111111111111111111111111111111111111"}, nil
		}},
		newPassStep(types.StepTest),
		&adaptiveCallStep{name: types.StepPush, fn: func(*StepContext) (*StepOutcome, error) {
			pushRan = true
			return &StepOutcome{}, nil
		}},
	}, nil)
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err == nil {
		t.Fatal("unresolved Test claim completed a run")
	}
	if pushRan {
		t.Fatal("unresolved Test claim reached Push")
	}
}

func TestExecutorExplicitExternalCIOwnerCarriesPendingClaim(t *testing.T) {
	database, p, baseRun, repo := setupTest(t)
	run, err := database.InsertRunWithExternalCIOwner(repo.ID, "feature-external-ci", baseRun.HeadSHA, baseRun.BaseSHA,
		nil, "", "", "", "develop", false, false, types.ExternalCIOwnerControllerShipPR, nil)
	if err != nil {
		t.Fatal(err)
	}
	workDir := completionFixture(t, database, run)
	_, err = database.BindRunPRContext(run.ID, db.PRContextCandidate{
		SourceRepo: "acme/repo", SourceBranch: "feature", LocalHeadSHA: run.HeadSHA, TargetBranch: "develop",
		TargetSHA:    "2222222222222222222222222222222222222222",
		MergeBaseSHA: "2222222222222222222222222222222222222222",
		DiffDigest:   "3333333333333333333333333333333333333333333333333333333333333333",
	}, types.StepRebase)
	if err != nil {
		t.Fatal(err)
	}
	const pending = `{"findings":[{"id":"review-ci-1","severity":"warning","description":"historical CI failure","action":"no-op","category":"review-support-pending","support":{"claim_type":"ci","ci":{"check_id":"old-check","head_sha":"1111111111111111111111111111111111111111"}}}]}`
	executor := NewExecutor(database, p, nil, nil, []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{Findings: pending, ReviewApprovedHeadSHA: run.HeadSHA}, nil
		}},
		newPassStep(types.StepPush), newPassStep(types.StepPR), newPassStep(types.StepCI),
	}, nil)
	executor.SetSkippedSteps([]types.StepName{types.StepPush, types.StepPR, types.StepCI})
	if err := executor.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("explicit external CI handoff was refused: %v", err)
	}
	current, err := database.GetRun(run.ID)
	if err != nil || current.Status != types.RunCompleted {
		t.Fatalf("run = %+v, err = %v; want completed with pending external CI", current, err)
	}
}

func TestExecutorExternalCIBypassRequiresHandoffSourceIdentity(t *testing.T) {
	database, p, baseRun, repo := setupTest(t)
	const head = "1111111111111111111111111111111111111111"
	run, err := database.InsertRunWithExternalCIOwner(repo.ID, "feature-external-ci-no-source", head, baseRun.BaseSHA,
		nil, "", "", "", "develop", false, false, types.ExternalCIOwnerControllerShipPR, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.BindRunPRContext(run.ID, db.PRContextCandidate{
		LocalHeadSHA: run.HeadSHA, TargetBranch: "develop",
		TargetSHA:    "2222222222222222222222222222222222222222",
		MergeBaseSHA: "2222222222222222222222222222222222222222",
		DiffDigest:   "3333333333333333333333333333333333333333333333333333333333333333",
	}, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	const pending = `{"findings":[{"id":"review-ci-no-source","severity":"warning","description":"historical CI failure","action":"no-op","category":"review-support-pending","support":{"claim_type":"ci","ci":{"check_id":"old-check","head_sha":"1111111111111111111111111111111111111111"}}}]}`
	executor := NewExecutor(database, p, nil, nil, []Step{
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{Findings: pending, ReviewApprovedHeadSHA: run.HeadSHA}, nil
		}},
		newPassStep(types.StepPush), newPassStep(types.StepPR), newPassStep(types.StepCI),
	}, nil)
	executor.SetSkippedSteps([]types.StepName{types.StepPush, types.StepPR, types.StepCI})
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err == nil {
		t.Fatal("external CI bypass completed without a handoff source identity")
	}
}

func TestExecutorExternalCIBypassRejectsPostReviewDocumentCommit(t *testing.T) {
	database, p, baseRun, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	reviewedHead, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, workDir, "docs.md", "post-review document update\n")
	execGit(t, workDir, "add", "docs.md")
	execGit(t, workDir, "commit", "-m", "document")
	advancedHead, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}

	run, err := database.InsertRunWithExternalCIOwner(repo.ID, "feature-external-ci-advanced", reviewedHead, baseRun.BaseSHA,
		nil, "", "", "", "develop", false, false, types.ExternalCIOwnerControllerShipPR, nil)
	if err != nil {
		t.Fatal(err)
	}
	receipt := db.PRContextCandidate{
		LocalHeadSHA: reviewedHead, TargetBranch: "develop",
		TargetSHA:    "2222222222222222222222222222222222222222",
		MergeBaseSHA: "2222222222222222222222222222222222222222",
		DiffDigest:   "3333333333333333333333333333333333333333333333333333333333333333",
	}
	if _, err := database.BindRunPRContext(run.ID, receipt, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	const pending = `{"findings":[{"id":"review-ci-advanced","severity":"warning","description":"historical CI failure","action":"no-op","category":"review-support-pending","support":{"claim_type":"ci","ci":{"check_id":"old-check","head_sha":"1111111111111111111111111111111111111111"}}}]}`
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(review.ID, pending); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, reviewedHead); err != nil {
		t.Fatal(err)
	}
	for _, name := range []types.StepName{types.StepPush, types.StepPR, types.StepCI} {
		step, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateStepStatus(step.ID, types.StepStatusSkipped); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.UpdateRunHeadSHA(run.ID, advancedHead); err != nil {
		t.Fatal(err)
	}
	receipt.LocalHeadSHA = advancedHead
	if _, err := database.AdvanceRunPRContext(run.ID, receipt); err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(database, p, nil, nil, nil, nil)
	executor.workDir = workDir
	if err := executor.validateReviewSupportOwners(run.ID, types.StepCI); err == nil {
		t.Fatal("external CI bypass accepted a document commit after Review approval")
	}
}

func TestExecutorTestSupportRejectsPostReviewDocumentCommitAtCompletion(t *testing.T) {
	database, p, baseRun, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	reviewedHead, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, workDir, "docs.md", "post-review document update\n")
	execGit(t, workDir, "add", "docs.md")
	execGit(t, workDir, "commit", "-m", "document")
	advancedHead, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}

	run, err := database.InsertRun(repo.ID, "feature-test-advanced", reviewedHead, baseRun.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	receipt := db.PRContextCandidate{
		LocalHeadSHA: reviewedHead, TargetBranch: "develop",
		TargetSHA:    "2222222222222222222222222222222222222222",
		MergeBaseSHA: "2222222222222222222222222222222222222222",
		DiffDigest:   "3333333333333333333333333333333333333333333333333333333333333333",
	}
	if _, err := database.BindRunPRContext(run.ID, receipt, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	bound, err := database.GetRunPRContext(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	claim := types.Finding{
		ID: "review-test-advanced", Severity: types.FindingSeverityWarning,
		Description: "historical test failure", Action: types.ActionNoOp,
		Category: types.FindingCategoryReviewSupportPending,
		Support:  &types.FindingSupport{ClaimType: types.FindingClaimTest, Test: &types.FindingTestSupport{Command: "go test ./..."}},
	}
	claimJSON, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{claim}})
	if err != nil {
		t.Fatal(err)
	}
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(review.ID, claimJSON); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	zero := 0
	resolved := claim
	resolved.ID = types.ReviewSupportClaimID(claim)
	resolved.Severity = types.FindingSeverityInfo
	resolved.Category = types.FindingCategoryReviewSupportResolved
	resolved.Support = &types.FindingSupport{
		ClaimType: types.FindingClaimTest, Test: &types.FindingTestSupport{Command: "go test ./..."},
		OwnerResult: &types.FindingOwnerResult{
			ReviewFindingID: claim.ID, HeadSHA: reviewedHead, TargetSHA: receipt.TargetSHA,
			DiffDigest: receipt.DiffDigest, Generation: bound.Generation,
			ObservedAt:  time.Now().UTC().Format(time.RFC3339Nano),
			Disposition: types.FindingSupportDispositionDisproven, ExitCode: &zero,
		},
	}
	resolvedJSON, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{resolved}})
	if err != nil {
		t.Fatal(err)
	}
	testStep, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(testStep.ID, resolvedJSON); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(testStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, reviewedHead); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(database, p, nil, nil, nil, nil)
	executor.workDir = workDir
	if err := executor.validateReviewSupportOwners(run.ID, types.StepTest); err != nil {
		t.Fatalf("current Test support was not accepted before the document commit: %v", err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, advancedHead); err != nil {
		t.Fatal(err)
	}
	receipt.LocalHeadSHA = advancedHead
	if _, err := database.AdvanceRunPRContext(run.ID, receipt); err != nil {
		t.Fatal(err)
	}

	if err := executor.validateReviewSupportOwners(run.ID, types.StepCI); err == nil {
		t.Fatal("completion accepted Test proof from before a document commit")
	}
}

func TestExecutorCIHistoricalClaimAcceptsFinalComparisonProof(t *testing.T) {
	database, p, baseRun, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	reviewedHead, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, workDir, "docs.md", "owned document correction\n")
	execGit(t, workDir, "add", "docs.md")
	execGit(t, workDir, "commit", "-m", "document")
	finalHead, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature-ci-forward", reviewedHead, baseRun.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	receipt := db.PRContextCandidate{LocalHeadSHA: reviewedHead, TargetBranch: "main", TargetSHA: reviewedHead,
		MergeBaseSHA: reviewedHead, DiffDigest: "3333333333333333333333333333333333333333333333333333333333333333"}
	if _, err := database.BindRunPRContext(run.ID, receipt, types.StepReview); err != nil {
		t.Fatal(err)
	}
	claim := types.Finding{ID: "review-ci-forward", Severity: types.FindingSeverityWarning, Action: types.ActionNoOp,
		Category: types.FindingCategoryReviewSupportPending, Description: "historical CI failure",
		Support: &types.FindingSupport{ClaimType: types.FindingClaimCI,
			CI: &types.FindingCISupport{CheckID: "bitbucket-status:build", HeadSHA: reviewedHead}}}
	claimJSON, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{claim}})
	if err != nil {
		t.Fatal(err)
	}
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(review.ID, claimJSON); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, reviewedHead); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, finalHead); err != nil {
		t.Fatal(err)
	}
	receipt.LocalHeadSHA = finalHead
	if _, err := database.AdvanceRunPRContext(run.ID, receipt); err != nil {
		t.Fatal(err)
	}
	current, err := database.GetRunPRContext(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	resolved := claim
	resolved.ID = types.ReviewSupportClaimID(claim)
	resolved.Category = types.FindingCategoryReviewSupportResolved
	resolved.Support = &types.FindingSupport{ClaimType: types.FindingClaimCI, CI: claim.Support.CI,
		OwnerResult: &types.FindingOwnerResult{ReviewFindingID: claim.ID, HeadSHA: finalHead,
			TargetSHA: current.TargetSHA, DiffDigest: current.DiffDigest, Generation: current.Generation,
			ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Disposition: types.FindingSupportDispositionDisproven, CheckState: "pass"}}
	resolvedJSON, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{resolved}})
	if err != nil {
		t.Fatal(err)
	}
	ci, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(ci.ID, resolvedJSON); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(ci.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(database, p, nil, nil, nil, nil)
	if err := executor.validateReviewSupportOwners(run.ID, types.StepCI); err != nil {
		t.Fatalf("final CI owner result refused: %v", err)
	}
	resolved.Support.OwnerResult.Generation--
	staleJSON, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{resolved}})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(ci.ID, staleJSON); err != nil {
		t.Fatal(err)
	}
	if err := executor.validateReviewSupportOwners(run.ID, types.StepCI); err == nil {
		t.Fatal("completion accepted stale CI proof")
	}
}

func TestExecutorRecoveredApprovalRevalidatesMovedPRTargetBeforeUsingParkedVerdict(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	intent, err := database.InsertStepResult(run.ID, types.StepIntent)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(intent.ID, types.StepStatusCompleted, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	rebase, err := database.InsertStepResult(run.ID, types.StepRebase)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStepWithStatus(rebase.ID, types.StepStatusCompleted, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(review.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"severity":"warning","description":"old comparison","action":"ask-user"}]}`
	if err := database.SetStepFindings(review.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(review.ID, 1, "initial", &findings, nil, "1111111111111111111111111111111111111111", 1); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(review.ID, types.StepStatusAwaitingApproval, 1); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var observed []types.StepName
	steps := []Step{
		newPassStep(types.StepIntent),
		&adaptiveCallStep{name: types.StepRebase, fn: func(*StepContext) (*StepOutcome, error) {
			observed = append(observed, types.StepRebase)
			return &StepOutcome{}, nil
		}},
		&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
			observed = append(observed, types.StepReview)
			return &StepOutcome{ReviewApprovedHeadSHA: "2222222222222222222222222222222222222222"}, nil
		}},
	}
	executor := NewExecutor(database, p, nil, nil, steps, nil)
	checks := 0
	executor.SetPRContextGuard(func(_ *StepContext, name types.StepName) (PRContextDecision, error) {
		checks++
		if checks == 1 {
			if err := database.ResetStepsFrom(run.ID, types.StepRebase.Order()); err != nil {
				return PRContextDecision{}, err
			}
			return PRContextDecision{Target: PRTargetSelection{TargetBranch: "develop"}, RestartFrom: types.StepRebase}, nil
		}
		return PRContextDecision{Target: PRTargetSelection{TargetBranch: "develop"}}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := executor.Resume(ctx, run, repo, workDir); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(observed, []types.StepName{types.StepRebase, types.StepReview}) {
		t.Fatalf("executed steps = %v, want fresh rebase and review", observed)
	}
	recoveredReview, err := database.GetStepResult(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredReview.Status != types.StepStatusCompleted {
		t.Fatalf("review status = %s", recoveredReview.Status)
	}
}
