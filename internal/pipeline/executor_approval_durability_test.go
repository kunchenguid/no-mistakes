package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutorApprovalGateParkFailureNeverPublishesTornGate(t *testing.T) {
	database, p, run, repo := setupTest(t)
	faultDB, err := sql.Open("sqlite", p.DB()+"?_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { faultDB.Close() })
	if _, err := faultDB.Exec(`
		CREATE TRIGGER reject_approval_step_park
		BEFORE UPDATE OF status ON step_results
		WHEN NEW.status = 'awaiting_approval'
		BEGIN
			SELECT RAISE(FAIL, 'injected approval step park failure');
		END;
	`); err != nil {
		t.Fatal(err)
	}

	step := newApprovalStep(types.StepReview, `{"findings":[{"id":"review-1","severity":"warning","description":"needs approval","action":"ask-user"}],"summary":"one"}`)
	executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = executor.Execute(ctx, run, repo, t.TempDir())
	if err == nil {
		t.Fatal("Execute accepted an approval gate whose atomic park write failed")
	}
	persistedRun, getErr := database.GetRun(run.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if persistedRun.AwaitingAgentSince != nil {
		t.Fatalf("failed gate left awaiting marker %d", *persistedRun.AwaitingAgentSince)
	}
	steps, getErr := database.GetStepsByRun(run.ID)
	if getErr != nil || len(steps) != 1 {
		t.Fatalf("steps = %+v, err = %v", steps, getErr)
	}
	if steps[0].Status == types.StepStatusAwaitingApproval {
		t.Fatal("failed gate exposed awaiting_approval without a complete transaction")
	}
}

func TestExecutorRespondJournalsBeforeAcknowledging(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := newApprovalStep(types.StepReview, `{"findings":[{"id":"review-1","severity":"warning","description":"needs approval","action":"ask-user"}],"summary":"one"}`)
	executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- executor.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	faultDB, err := sql.Open("sqlite", p.DB()+"?_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer faultDB.Close()
	if _, err := faultDB.Exec(`
		CREATE TRIGGER reject_approval_response
		BEFORE INSERT ON approval_actions
		BEGIN
			SELECT RAISE(FAIL, 'injected approval response failure');
		END;
	`); err != nil {
		t.Fatal(err)
	}
	if err := executor.Respond(types.StepReview, types.ActionApprove, nil); err == nil {
		t.Fatal("Respond acknowledged an action that was not durable")
	}
	parked, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.AwaitingAgentSince == nil {
		t.Fatal("failed response cleared the parked marker")
	}
	if _, err := faultDB.Exec(`DROP TRIGGER reject_approval_response`); err != nil {
		t.Fatal(err)
	}
	if err := executor.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("durable retry: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not consume durable response")
	}
}

func TestExecutorFixActionStaysReplayableUntilItsTransitionCommits(t *testing.T) {
	database, p, run, repo := setupTest(t)
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"needs approval","action":"ask-user"}],"summary":"one"}`
	executor := NewExecutor(database, p, nil, nil, []Step{newApprovalStep(types.StepReview, findings)}, nil)
	done := make(chan error, 1)
	go func() { done <- executor.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("steps = %+v, err = %v", steps, err)
	}
	step := steps[0]

	faultDB, err := sql.Open("sqlite", p.DB()+"?_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer faultDB.Close()
	if _, err := faultDB.Exec(`
		CREATE TRIGGER reject_fix_transition
		BEFORE UPDATE OF status ON step_results
		WHEN NEW.status = 'fixing'
		BEGIN
			SELECT RAISE(FAIL, 'injected fixing transition failure');
		END;
	`); err != nil {
		t.Fatal(err)
	}
	if err := executor.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "injected fixing transition failure") {
			t.Fatalf("Execute error = %v, want failed fixing transition", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not surface the failed fixing transition")
	}
	gate := waitForApprovalGate(t, database, step.ID)
	pending, err := database.GetPendingApprovalAction(gate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending == nil || pending.Action != types.ActionFix {
		t.Fatalf("failed fix transition consumed a non-replayable action: %+v", pending)
	}
	persistedRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedRun.AwaitingAgentSince == nil {
		t.Fatal("failed fix transition cleared the approval park")
	}
}

func TestExecutorSkipActionStaysReplayableUntilItsTransitionCommits(t *testing.T) {
	database, p, run, repo := setupTest(t)
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"needs approval","action":"ask-user"}],"summary":"one"}`
	executor := NewExecutor(database, p, nil, nil, []Step{newApprovalStep(types.StepReview, findings)}, nil)
	done := make(chan error, 1)
	go func() { done <- executor.Execute(context.Background(), run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("steps = %+v, err = %v", steps, err)
	}
	faultDB, err := sql.Open("sqlite", p.DB()+"?_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer faultDB.Close()
	if _, err := faultDB.Exec(`
		CREATE TRIGGER reject_skip_transition
		BEFORE UPDATE OF status ON step_results
		WHEN NEW.status = 'skipped'
		BEGIN
			SELECT RAISE(FAIL, 'injected skipped transition failure');
		END;
	`); err != nil {
		t.Fatal(err)
	}
	if err := executor.Respond(types.StepReview, types.ActionSkip, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "injected skipped transition failure") {
			t.Fatalf("Execute error = %v, want failed skipped transition", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not surface the failed skipped transition")
	}
	gate := waitForApprovalGate(t, database, steps[0].ID)
	pending, err := database.GetPendingApprovalAction(gate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending == nil || pending.Action != types.ActionSkip {
		t.Fatalf("failed skip transition consumed a non-replayable action: %+v", pending)
	}
	persistedRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedRun.AwaitingAgentSince == nil {
		t.Fatal("failed skip transition cleared the approval park")
	}
}

func TestExecutorResumeReplaysPersistedApprovalActions(t *testing.T) {
	for _, tc := range []struct {
		action   types.ApprovalAction
		wantStep types.StepStatus
		wantRun  types.RunStatus
		wantErr  bool
	}{
		{action: types.ActionApprove, wantStep: types.StepStatusCompleted, wantRun: types.RunCompleted},
		{action: types.ActionSkip, wantStep: types.StepStatusSkipped, wantRun: types.RunCompleted},
		{action: types.ActionAbort, wantStep: types.StepStatusFailed, wantRun: types.RunFailed, wantErr: true},
	} {
		t.Run(string(tc.action), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			step, err := database.InsertStepResult(run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			findings := `{"findings":[{"id":"review-1","severity":"warning","description":"needs approval","action":"ask-user"}],"summary":"one"}`
			round, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 17)
			if err != nil {
				t.Fatal(err)
			}
			gate, err := database.ParkApprovalGate(db.ParkApprovalGateInput{
				RunID: run.ID, StepResultID: step.ID, SourceRoundID: round.ID,
				Status: types.StepStatusAwaitingApproval, FindingsJSON: findings, DurationMS: 17,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.InsertApprovalAction(db.ApprovalActionInput{
				GateID: gate.ID, RunID: run.ID, StepResultID: step.ID, StepRoundID: round.ID,
				Action: tc.action, SelectedFindingIDsJSON: "null", InstructionsJSON: "null", AddedFindingsJSON: "null",
			}); err != nil {
				t.Fatal(err)
			}
			run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			executor := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, nil)
			err = executor.Resume(context.Background(), run, repo, t.TempDir())
			if (err != nil) != tc.wantErr {
				t.Fatalf("Resume() error = %v, wantErr %v", err, tc.wantErr)
			}
			persistedStep, getErr := database.GetStepResult(step.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			persistedRun, getErr := database.GetRun(run.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if persistedStep.Status != tc.wantStep || persistedRun.Status != tc.wantRun || persistedRun.AwaitingAgentSince != nil {
				t.Fatalf("replayed %s = step %s run %s parked %v, want step %s run %s unparked", tc.action, persistedStep.Status, persistedRun.Status, persistedRun.AwaitingAgentSince, tc.wantStep, tc.wantRun)
			}
			pending, getErr := database.GetPendingApprovalAction(gate.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if pending != nil {
				t.Fatalf("replayed %s left pending action %+v", tc.action, pending)
			}
		})
	}
}

func TestExecutorResumeReplaysAppliedFixAfterCrash(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	workDir := gitInitTestDir(t)
	headSHA, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, headSHA); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"needs approval","action":"ask-user"}],"summary":"one"}`
	round, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 17)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := database.ParkApprovalGate(db.ParkApprovalGateInput{
		RunID: run.ID, StepResultID: step.ID, SourceRoundID: round.ID,
		Status: types.StepStatusAwaitingApproval, FindingsJSON: findings, DurationMS: 17,
	})
	if err != nil {
		t.Fatal(err)
	}
	action, err := database.InsertApprovalAction(db.ApprovalActionInput{
		GateID: gate.ID, RunID: run.ID, StepResultID: step.ID, StepRoundID: round.ID,
		Action: types.ActionFix, SelectedFindingIDsJSON: `["review-1"]`, InstructionsJSON: `{}`, AddedFindingsJSON: `[]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	selected := `["review-1"]`
	if err := database.ApplyApprovalFix(db.ApplyApprovalFixInput{ActionID: action.ID, ParkedMS: 5, SelectedIDsJSON: &selected}); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(database, p, nil, nil, []Step{newApprovalStep(types.StepReview, findings)}, nil)
	done := make(chan error, 1)
	go func() { done <- executor.ResumeRecoveredPrefix(context.Background(), run, repo, workDir) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixReview)
	if err := executor.Respond(types.StepReview, types.ActionSkip, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ResumeRecoveredPrefix: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovered applied fix did not reach its next approval gate")
	}
	persistedRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedStep, err := database.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedRun.Status != types.RunCompleted || persistedStep.Status != types.StepStatusSkipped {
		t.Fatalf("applied-fix recovery = run %s step %s, want completed/skipped", persistedRun.Status, persistedStep.Status)
	}
}

func TestExecutorResumeAppliedFixContinuesAfterDurableRepairFrontier(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	workDir := gitInitTestDir(t)
	headSHA, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, headSHA); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"test-1","severity":"error","description":"tests require repair","action":"ask-user"}],"summary":"one"}`
	sourceRound, err := database.InsertStepRound(stepResult.ID, 1, "initial", &findings, nil, 11)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := database.ParkApprovalGate(db.ParkApprovalGateInput{
		RunID: run.ID, StepResultID: stepResult.ID, SourceRoundID: sourceRound.ID,
		Status: types.StepStatusAwaitingApproval, FindingsJSON: findings,
		RepairChecksJSON: `[]`, DurationMS: 11,
	})
	if err != nil {
		t.Fatal(err)
	}
	action, err := database.InsertApprovalAction(db.ApprovalActionInput{
		GateID: gate.ID, RunID: run.ID, StepResultID: stepResult.ID, StepRoundID: sourceRound.ID,
		Action: types.ActionFix, SelectedFindingIDsJSON: `["test-1"]`, InstructionsJSON: `{}`, AddedFindingsJSON: `[]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	selected := `["test-1"]`
	if err := database.ApplyApprovalFix(db.ApplyApprovalFixInput{ActionID: action.ID, ParkedMS: 5, SelectedIDsJSON: &selected}); err != nil {
		t.Fatal(err)
	}

	repairRound, err := database.ReserveStepRound(stepResult.ID, 2, "auto_fix")
	if err != nil {
		t.Fatal(err)
	}
	lineageID := fmt.Sprintf("approval:%s:test-1", action.ID)
	repairID, err := database.StartFindingRepair(db.FindingRepairStart{
		RunID: run.ID, LineageID: lineageID, StepResultID: stepResult.ID, StepRoundID: repairRound.ID,
		Severity: "error", Action: types.ActionAskUser, Description: "tests require repair", Tier: 0, RemainingBudget: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := types.InvocationScope{
		Kind: types.InvocationScopePipeline, RunID: run.ID,
		StepResultID: stepResult.ID, StepRoundID: repairRound.ID,
	}
	startSucceededAttempt := func(purpose types.Purpose, role types.InvocationRole, key, profile string) string {
		t.Helper()
		id, startErr := database.StartInvocationAttempt(types.InvocationAttemptStart{
			Purpose: purpose, Role: role, Scope: scope, CandidateKey: key,
			Candidate: types.InvocationCandidate{
				Profile: profile, Runner: types.RunnerCodex, Model: "test-model", Effort: types.EffortHigh,
			},
		})
		if startErr != nil {
			t.Fatal(startErr)
		}
		if finishErr := database.FinishInvocationAttempt(id, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded}); finishErr != nil {
			t.Fatal(finishErr)
		}
		return id
	}
	fixerAttempt := startSucceededAttempt(types.PurposeUnstructuredTestRepair, types.InvocationRoleFixer, "fix_balanced:0:codex", "fix_balanced")
	verifierAttempt := startSucceededAttempt(types.PurposeNormalAggregateVerification, types.InvocationRoleVerifier, "review_strong:0:codex", "review_strong")
	if err := database.SetFindingRepairFixer(repairID, fixerAttempt); err != nil {
		t.Fatal(err)
	}
	if err := database.SetFindingRepairVerifier(repairID, verifierAttempt); err != nil {
		t.Fatal(err)
	}
	if err := database.ResolveFindingRepair(repairID, db.RepairVerdictUnresolved, "still failing", db.RepairStatusUnresolved); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteReservedStepRound(repairRound.ID, nil, nil, 7); err != nil {
		t.Fatal(err)
	}

	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	stepCalls := 0
	step := &adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
		stepCalls++
		return &StepOutcome{}, nil
	}}
	executor := NewExecutor(
		database,
		p,
		&config.Config{Routing: config.DefaultRoutingConfig()},
		&scriptedExecutorRepairAgent{resolve: true},
		[]Step{step},
		nil,
	)
	if err := executor.ResumeRecoveredPrefix(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("ResumeRecoveredPrefix: %v", err)
	}
	if stepCalls != 0 {
		t.Fatalf("recovery reran original Test step %d time(s), want durable repair continuation", stepCalls)
	}
	repairs, err := database.GetFindingRepairsByLineage(lineageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repairs) != 2 ||
		repairs[0].Tier != 0 || repairs[0].RemainingBudget != 1 || repairs[0].Status != db.RepairStatusUnresolved ||
		repairs[1].Tier != 1 || repairs[1].RemainingBudget != 0 || repairs[1].Status != db.RepairStatusResolved {
		t.Fatalf("recovered repair frontier = %+v, want tier 0 unresolved then tier 1 resolved without duplicate budget", repairs)
	}
	rounds, err := database.GetRoundsByStep(stepResult.ID)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[int]bool, len(rounds))
	for _, round := range rounds {
		if seen[round.Round] {
			t.Fatalf("recovery duplicated round %d: %+v", round.Round, rounds)
		}
		seen[round.Round] = true
	}
}

func TestExecutorRecoveredVerifyRepairReentersAggregateBeforePush(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	workDir := gitInitTestDir(t)
	if _, err := git.Run(context.Background(), workDir, "checkout", "-b", run.Branch); err != nil {
		t.Fatal(err)
	}
	headSHA, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, headSHA); err != nil {
		t.Fatal(err)
	}
	verifyResult, err := database.InsertStepResult(run.ID, types.StepVerify)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepResult(run.ID, types.StepPush); err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(verifyResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"verify-1","severity":"error","description":"aggregate verification failed","action":"ask-user"}],"summary":"one"}`
	sourceRound, err := database.InsertStepRound(verifyResult.ID, 1, "initial", &findings, nil, 11)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := database.ParkApprovalGate(db.ParkApprovalGateInput{
		RunID: run.ID, StepResultID: verifyResult.ID, SourceRoundID: sourceRound.ID,
		Status: types.StepStatusAwaitingApproval, FindingsJSON: findings,
		RepairChecksJSON: `[]`, DurationMS: 11,
	})
	if err != nil {
		t.Fatal(err)
	}
	action, err := database.InsertApprovalAction(db.ApprovalActionInput{
		GateID: gate.ID, RunID: run.ID, StepResultID: verifyResult.ID, StepRoundID: sourceRound.ID,
		Action: types.ActionFix, SelectedFindingIDsJSON: `["verify-1"]`, InstructionsJSON: `{}`, AddedFindingsJSON: `[]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	selected := `["verify-1"]`
	if err := database.ApplyApprovalFix(db.ApplyApprovalFixInput{ActionID: action.ID, ParkedMS: 5, SelectedIDsJSON: &selected}); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	aggregateCalls := 0
	verify := &adaptiveCallStep{name: types.StepVerify, fn: func(*StepContext) (*StepOutcome, error) {
		aggregateCalls++
		candidate, headErr := git.HeadSHA(context.Background(), workDir)
		if headErr != nil {
			return nil, headErr
		}
		if _, sealErr := database.CreateSeal(run.ID, candidate, "reviewed"); sealErr != nil {
			return nil, sealErr
		}
		return &StepOutcome{}, nil
	}}
	push := newPassStep(types.StepPush)
	repairAgent := &scriptedExecutorRepairAgent{
		resolve: true,
		fixEdit: func(cwd string) {
			writeTestFile(t, cwd, "verify-recovery-fix.txt", "fixed\n")
		},
	}
	executor := NewExecutor(
		database,
		p,
		&config.Config{Routing: config.DefaultRoutingConfig()},
		repairAgent,
		[]Step{verify, push},
		nil,
	)
	if err := executor.ResumeRecoveredPrefix(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("ResumeRecoveredPrefix: %v", err)
	}
	if aggregateCalls != 1 || push.callCount() != 1 {
		t.Fatalf("recovered Verify repair ran aggregate %d time(s), Push %d time(s); want 1 then 1", aggregateCalls, push.callCount())
	}
	seal, err := database.LatestSeal(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	reviewed, err := database.LatestSealByReason(run.ID, "reviewed")
	if err != nil || seal == nil || reviewed == nil || reviewed.SHA != seal.SHA {
		t.Fatalf("recovered publication evidence = seal %+v reviewed %+v err %v", seal, reviewed, err)
	}
}

func TestExecutorRecoveryReconcilesDurableAggregateFrontierWithoutDuplicateRound(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	workDir := gitInitTestDir(t)
	if _, err := git.Run(context.Background(), workDir, "checkout", "-b", run.Branch); err != nil {
		t.Fatal(err)
	}
	initialHead, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, initialHead); err != nil {
		t.Fatal(err)
	}
	verifyResult, err := database.InsertStepResult(run.ID, types.StepVerify)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepResult(run.ID, types.StepPush); err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(verifyResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"verify-1","severity":"error","description":"aggregate verification failed","action":"ask-user"}],"summary":"one"}`
	sourceRound, err := database.InsertStepRound(verifyResult.ID, 1, "initial", &findings, nil, 11)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := database.ParkApprovalGate(db.ParkApprovalGateInput{
		RunID: run.ID, StepResultID: verifyResult.ID, SourceRoundID: sourceRound.ID,
		Status: types.StepStatusAwaitingApproval, FindingsJSON: findings,
		RepairChecksJSON: `[]`, DurationMS: 11,
	})
	if err != nil {
		t.Fatal(err)
	}
	action, err := database.InsertApprovalAction(db.ApprovalActionInput{
		GateID: gate.ID, RunID: run.ID, StepResultID: verifyResult.ID, StepRoundID: sourceRound.ID,
		Action: types.ActionFix, SelectedFindingIDsJSON: `["verify-1"]`, InstructionsJSON: `{}`, AddedFindingsJSON: `[]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	selected := `["verify-1"]`
	if err := database.ApplyApprovalFix(db.ApplyApprovalFixInput{ActionID: action.ID, ParkedMS: 5, SelectedIDsJSON: &selected}); err != nil {
		t.Fatal(err)
	}
	repairRound, err := database.ReserveStepRound(verifyResult.ID, 2, "auto_fix")
	if err != nil {
		t.Fatal(err)
	}
	lineageID := fmt.Sprintf("approval:%s:verify-1", action.ID)
	repairID, err := database.StartFindingRepair(db.FindingRepairStart{
		RunID: run.ID, LineageID: lineageID, StepResultID: verifyResult.ID, StepRoundID: repairRound.ID,
		Severity: "error", Action: types.ActionAskUser, Description: "aggregate verification failed",
		Tier: 0, RemainingBudget: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	startSucceeded := func(roundID string, purpose types.Purpose, role types.InvocationRole, key, profile string) string {
		t.Helper()
		id, startErr := database.StartInvocationAttempt(types.InvocationAttemptStart{
			Purpose: purpose, Role: role,
			Scope: types.InvocationScope{
				Kind: types.InvocationScopePipeline, RunID: run.ID,
				StepResultID: verifyResult.ID, StepRoundID: roundID,
			},
			CandidateKey: key,
			Candidate: types.InvocationCandidate{
				Profile: profile, Runner: types.RunnerCodex, Model: "test-model", Effort: types.EffortHigh,
			},
		})
		if startErr != nil {
			t.Fatal(startErr)
		}
		if finishErr := database.FinishInvocationAttempt(id, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded}); finishErr != nil {
			t.Fatal(finishErr)
		}
		return id
	}
	fixerAttempt := startSucceeded(repairRound.ID, types.PurposeStructuredFindingRepair, types.InvocationRoleFixer, "fix_fast:0:codex", "fix_fast")
	repairVerifier := startSucceeded(repairRound.ID, types.PurposeNormalAggregateVerification, types.InvocationRoleVerifier, "review_strong:0:codex", "review_strong")
	if err := database.SetFindingRepairFixer(repairID, fixerAttempt); err != nil {
		t.Fatal(err)
	}
	if err := database.SetFindingRepairVerifier(repairID, repairVerifier); err != nil {
		t.Fatal(err)
	}
	if err := database.ResolveFindingRepair(repairID, db.RepairVerdictResolved, "fixed", db.RepairStatusResolved); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteReservedStepRound(repairRound.ID, nil, nil, 7); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, workDir, "recovered-aggregate.txt", "fixed\n")
	if _, err := git.Run(context.Background(), workDir, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(context.Background(), workDir, "commit", "-m", "repair Verify finding"); err != nil {
		t.Fatal(err)
	}
	repairedHead, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, repairedHead); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateSeal(run.ID, repairedHead, "pre_verify"); err != nil {
		t.Fatal(err)
	}
	aggregateRound, err := database.ReserveStepRound(verifyResult.ID, 3, "auto_fix")
	if err != nil {
		t.Fatal(err)
	}
	startSucceeded(
		aggregateRound.ID,
		types.PurposeEscalatedAggregateVerification,
		types.InvocationRoleVerifier,
		"authority_strong:0:codex",
		"authority_strong",
	)
	if _, err := database.CreateSeal(run.ID, repairedHead, "reviewed"); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	aggregateCalls := 0
	verify := &adaptiveCallStep{name: types.StepVerify, fn: func(*StepContext) (*StepOutcome, error) {
		aggregateCalls++
		return &StepOutcome{}, nil
	}}
	push := newPassStep(types.StepPush)
	executor := NewExecutor(
		database,
		p,
		&config.Config{Routing: config.DefaultRoutingConfig()},
		&scriptedExecutorRepairAgent{resolve: true},
		[]Step{verify, push},
		nil,
	)
	if err := executor.ResumeRecoveredPrefix(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("ResumeRecoveredPrefix: %v", err)
	}
	if aggregateCalls != 0 || push.callCount() != 1 {
		t.Fatalf("reconciled aggregate frontier reran Verify %d time(s), Push %d time(s); want 0 then 1", aggregateCalls, push.callCount())
	}
	rounds, err := database.GetAllRoundsByStep(verifyResult.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 3 || rounds[2].ID != aggregateRound.ID || rounds[2].State != db.StepRoundCompleted {
		t.Fatalf("reconciled rounds = %+v, want the original three-round frontier completed in place", rounds)
	}
}

func TestExecutorRecoveryCancelsEveryUnreconstructableOpenChildBoundary(t *testing.T) {
	for _, withActiveFixer := range []bool{false, true} {
		name := "reserved"
		if withActiveFixer {
			name = "fixer_started"
		}
		t.Run(name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			workDir := gitInitTestDir(t)
			headSHA, err := git.HeadSHA(context.Background(), workDir)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunHeadSHA(run.ID, headSHA); err != nil {
				t.Fatal(err)
			}
			stepResult, err := database.InsertStepResult(run.ID, types.StepTest)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.StartStep(stepResult.ID); err != nil {
				t.Fatal(err)
			}
			findings := `{"findings":[{"id":"test-1","severity":"error","description":"tests require repair","action":"ask-user"}],"summary":"one"}`
			sourceRound, err := database.InsertStepRound(stepResult.ID, 1, "initial", &findings, nil, 11)
			if err != nil {
				t.Fatal(err)
			}
			gate, err := database.ParkApprovalGate(db.ParkApprovalGateInput{
				RunID: run.ID, StepResultID: stepResult.ID, SourceRoundID: sourceRound.ID,
				Status: types.StepStatusAwaitingApproval, FindingsJSON: findings,
				RepairChecksJSON: `[]`, DurationMS: 11,
			})
			if err != nil {
				t.Fatal(err)
			}
			action, err := database.InsertApprovalAction(db.ApprovalActionInput{
				GateID: gate.ID, RunID: run.ID, StepResultID: stepResult.ID, StepRoundID: sourceRound.ID,
				Action: types.ActionFix, SelectedFindingIDsJSON: `["test-1"]`, InstructionsJSON: `{}`, AddedFindingsJSON: `[]`,
			})
			if err != nil {
				t.Fatal(err)
			}
			selected := `["test-1"]`
			if err := database.ApplyApprovalFix(db.ApplyApprovalFixInput{ActionID: action.ID, ParkedMS: 5, SelectedIDsJSON: &selected}); err != nil {
				t.Fatal(err)
			}
			openRound, err := database.ReserveStepRound(stepResult.ID, 2, "auto_fix")
			if err != nil {
				t.Fatal(err)
			}
			activeAttemptID := ""
			if withActiveFixer {
				activeAttemptID, err = database.StartInvocationAttempt(types.InvocationAttemptStart{
					Purpose: types.PurposeUnstructuredTestRepair,
					Role:    types.InvocationRoleFixer,
					Scope: types.InvocationScope{
						Kind: types.InvocationScopePipeline, RunID: run.ID,
						StepResultID: stepResult.ID, StepRoundID: openRound.ID,
					},
					CandidateKey: "fix_balanced:0:codex",
					Candidate: types.InvocationCandidate{
						Profile: "fix_balanced", Runner: types.RunnerCodex, Model: "test-model", Effort: types.EffortHigh,
					},
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			executor := NewExecutor(
				database,
				p,
				&config.Config{Routing: config.DefaultRoutingConfig()},
				&scriptedExecutorRepairAgent{resolve: true},
				[]Step{newPassStep(types.StepTest)},
				nil,
			)
			if err := executor.ResumeRecoveredPrefix(context.Background(), run, repo, workDir); err != nil {
				t.Fatalf("ResumeRecoveredPrefix: %v", err)
			}
			reconciled, err := database.GetStepRound(openRound.ID)
			if err != nil {
				t.Fatal(err)
			}
			if reconciled == nil || reconciled.State != db.StepRoundCancelled {
				t.Fatalf("unreconstructable child = %+v, want cancelled in place", reconciled)
			}
			if activeAttemptID != "" {
				attempt, err := database.GetInvocationAttempt(activeAttemptID)
				if err != nil {
					t.Fatal(err)
				}
				if attempt == nil || attempt.Terminal == nil || attempt.Terminal.Outcome != types.InvocationOutcomeInterrupted {
					t.Fatalf("recovered active attempt = %+v, want interrupted terminal", attempt)
				}
			}
			rounds, err := database.GetAllRoundsByStep(stepResult.ID)
			if err != nil {
				t.Fatal(err)
			}
			seen := make(map[int]bool, len(rounds))
			for _, round := range rounds {
				if round.State == db.StepRoundReserved {
					t.Fatalf("recovery preserved open child round %+v", round)
				}
				if seen[round.Round] {
					t.Fatalf("recovery duplicated round ordinal %d: %+v", round.Round, rounds)
				}
				seen[round.Round] = true
			}
		})
	}
}

func approvalActionInput(gate *db.ApprovalGate, action types.ApprovalAction) db.ApprovalActionInput {
	return db.ApprovalActionInput{
		GateID: gate.ID, RunID: gate.RunID, StepResultID: gate.StepResultID, StepRoundID: gate.SourceRoundID,
		Action: action, SelectedFindingIDsJSON: "null", InstructionsJSON: "null", AddedFindingsJSON: "null",
	}
}

func waitForApprovalGate(t *testing.T, database *db.DB, stepResultID string) *db.ApprovalGate {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		gate, err := database.GetCurrentApprovalGate(stepResultID)
		if err != nil {
			t.Fatal(err)
		}
		if gate != nil {
			return gate
		}
		if time.Now().After(deadline) {
			t.Fatal(fmt.Sprintf("approval gate for %s was never persisted", stepResultID))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func TestExecutorResumeReplaysPersistedNonReviewFix(t *testing.T) {
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
	findings := `{"findings":[{"id":"test-1","severity":"error","description":"tests require repair","action":"ask-user"}],"summary":"one"}`
	round, err := database.InsertStepRound(stepResult.ID, 1, "initial", &findings, nil, 11)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := database.ParkApprovalGate(db.ParkApprovalGateInput{
		RunID: run.ID, StepResultID: stepResult.ID, SourceRoundID: round.ID,
		Status: types.StepStatusAwaitingApproval, FindingsJSON: findings,
		RepairChecksJSON: `[{"kind":"shell","command":"go version"}]`, DurationMS: 11,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertApprovalAction(db.ApprovalActionInput{
		GateID: gate.ID, RunID: run.ID, StepResultID: stepResult.ID, StepRoundID: round.ID,
		Action: types.ActionFix, SelectedFindingIDsJSON: `["test-1"]`, InstructionsJSON: `{}`, AddedFindingsJSON: `[]`,
	}); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	workDir := gitInitTestDir(t)
	repairAgent := &scriptedExecutorRepairAgent{resolve: true}
	stepCalls := 0
	step := &adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
		stepCalls++
		return &StepOutcome{}, nil
	}}
	executor := NewExecutor(
		database,
		p,
		&config.Config{Routing: config.DefaultRoutingConfig()},
		repairAgent,
		[]Step{step},
		nil,
	)
	if err := executor.Resume(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Resume persisted fix: %v", err)
	}
	if stepCalls != 0 {
		t.Fatalf("replayed durable fix reran the original Test step %d time(s), want repair journal replay", stepCalls)
	}
	repairs, err := database.GetFindingRepairsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repairs) != 1 || repairs[0].Status != db.RepairStatusResolved {
		t.Fatalf("replayed repair cycles = %+v, want one resolved cycle", repairs)
	}
	checks, err := database.GetFindingRepairChecks(repairs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || checks[0].Command != "go version" || !checks[0].Applicable || checks[0].ExitCode != 0 {
		t.Fatalf("replayed deterministic checks = %+v, want successful persisted go version check", checks)
	}
	persistedRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persistedRun.Status != types.RunCompleted || persistedRun.AwaitingAgentSince != nil {
		t.Fatalf("replayed fix run = status %s parked %v", persistedRun.Status, persistedRun.AwaitingAgentSince)
	}
}

func TestExecutorNonReviewUserFixRequiresDurableVerifiedRepair(t *testing.T) {
	for _, stepName := range []types.StepName{types.StepTest, types.StepDocument, types.StepLint, types.StepVerify} {
		t.Run(string(stepName), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := gitInitTestDir(t)
			findingID := string(stepName) + "-1"
			findings := fmt.Sprintf(
				`{"findings":[{"id":%q,"severity":"error","description":%q,"action":"ask-user"}],"summary":"one"}`,
				findingID,
				string(stepName)+" requires a user-authorized repair",
			)
			liveCheckCalls := 0
			repairChecks := make([]RepairCheck, 0, 1)
			switch stepName {
			case types.StepTest, types.StepLint:
				repairChecks = append(repairChecks, RepairCheck{
					Command: "go version",
					Run: func(context.Context) (bool, int, string) {
						liveCheckCalls++
						return true, 0, "go version"
					},
				})
			case types.StepDocument:
				repairChecks = append(repairChecks, RepairCheck{
					Command: "git diff --check",
					Run: func(context.Context) (bool, int, string) {
						liveCheckCalls++
						return true, 0, ""
					},
				})
			}
			stepCalls := 0
			step := &adaptiveCallStep{name: stepName, fn: func(*StepContext) (*StepOutcome, error) {
				stepCalls++
				if stepName == types.StepVerify && stepCalls > 1 {
					head, err := git.HeadSHA(context.Background(), workDir)
					if err != nil {
						return nil, err
					}
					if _, err := database.CreateSeal(run.ID, head, "reviewed"); err != nil {
						return nil, err
					}
					return &StepOutcome{}, nil
				}
				return &StepOutcome{NeedsApproval: true, Findings: findings, RepairChecks: repairChecks}, nil
			}}
			repairAgent := &scriptedExecutorRepairAgent{resolve: true}
			executor := NewExecutor(
				database,
				p,
				&config.Config{Routing: config.DefaultRoutingConfig()},
				repairAgent,
				[]Step{step},
				nil,
			)
			done := make(chan error, 1)
			go func() { done <- executor.Execute(context.Background(), run, repo, workDir) }()
			waitForStepStatus(t, database, run.ID, stepName, types.StepStatusAwaitingApproval)
			steps, err := database.GetStepsByRun(run.ID)
			if err != nil || len(steps) != 1 {
				t.Fatalf("parked step = %+v, err = %v", steps, err)
			}
			gate := waitForApprovalGate(t, database, steps[0].ID)
			if (len(repairChecks) == 0) != (gate.RepairChecksJSON == "[]") {
				t.Fatalf("durable %s repair checks = %s, want %d identities", stepName, gate.RepairChecksJSON, len(repairChecks))
			}
			if err := executor.Respond(stepName, types.ActionFix, []string{findingID}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Execute: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("verified user fix timed out")
			}
			if stepName == types.StepVerify {
				if stepCalls != 2 {
					t.Fatalf("Verify calls = %d, want consented repair followed by fresh aggregate Verify", stepCalls)
				}
				seal, err := database.LatestSeal(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				reviewed, err := database.LatestSealByReason(run.ID, "reviewed")
				if err != nil || seal == nil || reviewed == nil || reviewed.SHA != seal.SHA {
					t.Fatalf("consented Verify publication evidence = seal %+v reviewed %+v err %v", seal, reviewed, err)
				}
			}
			repairs, err := database.GetFindingRepairsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(repairs) == 0 {
				t.Fatal("user fix completed without a durable repair cycle")
			}
			for _, repair := range repairs {
				if repair.Status != db.RepairStatusResolved ||
					repair.Verdict != db.RepairVerdictResolved ||
					repair.FixerAttemptID == "" ||
					repair.VerifierAttemptID == "" {
					t.Fatalf("repair = %+v, want independently verified resolved cycle", repair)
				}
			}
			recordedChecks, err := database.GetFindingRepairChecks(repairs[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if liveCheckCalls != len(repairChecks) || len(recordedChecks) != len(repairChecks) {
				t.Fatalf("%s live checks = calls %d journal %+v, want %d identical checks", stepName, liveCheckCalls, recordedChecks, len(repairChecks))
			}
			persistedRun, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			unresolved, err := database.HasUnresolvedBlockingRepair(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persistedRun.Status != types.RunCompleted || unresolved {
				t.Fatalf("completed user fix = run %s unresolved %v", persistedRun.Status, unresolved)
			}
		})
	}
}

func TestExecutorResumeFinalizesRejectedApprovalAfterCrash(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"error","description":"repair exhausted","action":"auto-fix"}],"summary":"one"}`
	round, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 17)
	if err != nil {
		t.Fatal(err)
	}
	repair, err := database.StartFindingRepair(db.FindingRepairStart{
		RunID: run.ID, LineageID: "review-unresolved", StepResultID: step.ID, StepRoundID: round.ID,
		Severity: "error", Action: types.ActionAutoFix, Description: "repair exhausted", Tier: 0, RemainingBudget: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ResolveFindingRepair(repair, db.RepairVerdictUnresolved, "still failing", db.RepairStatusUnresolved); err != nil {
		t.Fatal(err)
	}
	gate, err := database.ParkApprovalGate(db.ParkApprovalGateInput{
		RunID: run.ID, StepResultID: step.ID, SourceRoundID: round.ID,
		Status: types.StepStatusAwaitingApproval, FindingsJSON: findings, DurationMS: 17,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertApprovalAction(approvalActionInput(gate, types.ActionApprove)); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, nil)
	err = executor.Resume(context.Background(), run, repo, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "cannot be approved") {
		t.Fatalf("Resume error = %v, want unresolved approval rejection", err)
	}
	rejectedStep, err := database.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	rejectedRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := database.GetPendingApprovalAction(gate.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentGate, err := database.GetCurrentApprovalGate(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rejectedStep.Status != types.StepStatusFailed || rejectedRun.Status != types.RunFailed || rejectedRun.AwaitingAgentSince != nil {
		t.Fatalf("replayed rejection = step %s run %s parked %v, want terminal failed and unparked", rejectedStep.Status, rejectedRun.Status, rejectedRun.AwaitingAgentSince)
	}
	if pending != nil || currentGate != nil {
		t.Fatalf("replayed rejection left pending action %+v or current gate %+v", pending, currentGate)
	}
}

func TestExecutorResumeRestoresEveryNonReviewRepairCheckSet(t *testing.T) {
	for _, tc := range []struct {
		stepName      types.StepName
		repairChecks  func(string) string
		wantCheckRuns int
	}{
		{stepName: types.StepLint, repairChecks: func(string) string {
			return `[{"kind":"shell","command":"go version"}]`
		}, wantCheckRuns: 1},
		{stepName: types.StepDocument, repairChecks: func(baseSHA string) string {
			return fmt.Sprintf(`[{"kind":"document_diff","command":"git diff --check","base_sha":%q}]`, baseSHA)
		}, wantCheckRuns: 1},
		{stepName: types.StepVerify, repairChecks: func(string) string { return `[]` }},
	} {
		t.Run(string(tc.stepName), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			workDir := gitInitTestDir(t)
			headSHA, err := git.HeadSHA(context.Background(), workDir)
			if err != nil {
				t.Fatal(err)
			}
			stepResult, err := database.InsertStepResult(run.ID, tc.stepName)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.StartStep(stepResult.ID); err != nil {
				t.Fatal(err)
			}
			findingID := string(tc.stepName) + "-1"
			findings := fmt.Sprintf(
				`{"findings":[{"id":%q,"severity":"error","description":"repair after crash","action":"ask-user"}],"summary":"one"}`,
				findingID,
			)
			round, err := database.InsertStepRound(stepResult.ID, 1, "initial", &findings, nil, 11)
			if err != nil {
				t.Fatal(err)
			}
			gate, err := database.ParkApprovalGate(db.ParkApprovalGateInput{
				RunID: run.ID, StepResultID: stepResult.ID, SourceRoundID: round.ID,
				Status: types.StepStatusAwaitingApproval, FindingsJSON: findings,
				RepairChecksJSON: tc.repairChecks(headSHA), DurationMS: 11,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.InsertApprovalAction(db.ApprovalActionInput{
				GateID: gate.ID, RunID: run.ID, StepResultID: stepResult.ID, StepRoundID: round.ID,
				Action: types.ActionFix, SelectedFindingIDsJSON: fmt.Sprintf("[%q]", findingID), InstructionsJSON: `{}`, AddedFindingsJSON: `[]`,
			}); err != nil {
				t.Fatal(err)
			}
			run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			executor := NewExecutor(
				database,
				p,
				&config.Config{Routing: config.DefaultRoutingConfig()},
				&scriptedExecutorRepairAgent{resolve: true},
				[]Step{newPassStep(tc.stepName)},
				nil,
			)
			if err := executor.Resume(context.Background(), run, repo, workDir); err != nil {
				t.Fatalf("Resume persisted %s fix: %v", tc.stepName, err)
			}
			repairs, err := database.GetFindingRepairsByRun(run.ID)
			if err != nil || len(repairs) != 1 || repairs[0].Status != db.RepairStatusResolved {
				t.Fatalf("replayed %s repairs = %+v, err = %v", tc.stepName, repairs, err)
			}
			checks, err := database.GetFindingRepairChecks(repairs[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(checks) != tc.wantCheckRuns {
				t.Fatalf("replayed %s deterministic checks = %+v, want %d", tc.stepName, checks, tc.wantCheckRuns)
			}
		})
	}
}

func TestDecodeDurableRepairChecksPreservesEmptyVerifySet(t *testing.T) {
	checks, err := decodeDurableRepairChecks(types.StepVerify, `[]`, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if checks == nil || len(checks) != 0 {
		t.Fatalf("recovered Verify checks = %#v, want non-nil empty set", checks)
	}
}
