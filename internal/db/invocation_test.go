package db

import (
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestInvocationAttemptJournalIsAppendOnlyAndSecretFree(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/invocations", "git@github.com:user/invocations.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	round, err := d.ReserveStepRound(step.ID, 1, "initial")
	if err != nil {
		t.Fatalf("reserve round: %v", err)
	}

	start := types.InvocationAttemptStart{
		Purpose:      types.PurposeInitialReview,
		Role:         types.InvocationRoleVerifier,
		Scope:        types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: run.ID, StepResultID: step.ID, StepRoundID: round.ID},
		CandidateKey: types.LegacyCandidateKey,
	}
	attemptID, err := d.StartInvocationAttempt(start)
	if err != nil {
		t.Fatalf("start invocation: %v", err)
	}
	active, err := d.GetInvocationAttempt(attemptID)
	if err != nil {
		t.Fatalf("get active invocation: %v", err)
	}
	if active == nil || active.Terminal != nil {
		t.Fatalf("active invocation = %+v, want start without terminal", active)
	}
	if active.Start != start {
		t.Fatalf("stored start = %+v, want %+v", active.Start, start)
	}

	terminal := types.InvocationAttemptTerminal{
		Outcome:             types.InvocationOutcomeSucceeded,
		DurationMS:          1234,
		InputTokens:         12,
		OutputTokens:        8,
		CacheReadTokens:     5,
		CacheCreationTokens: 2,
	}
	if err := d.FinishInvocationAttempt(attemptID, terminal); err != nil {
		t.Fatalf("finish invocation: %v", err)
	}
	completed, err := d.GetInvocationAttempt(attemptID)
	if err != nil {
		t.Fatalf("get completed invocation: %v", err)
	}
	if completed == nil || completed.Terminal == nil || *completed.Terminal != terminal {
		t.Fatalf("completed invocation = %+v, want terminal %+v", completed, terminal)
	}
	if completed.Start != start {
		t.Fatalf("start changed after terminal append: got %+v want %+v", completed.Start, start)
	}
	if err := d.FinishInvocationAttempt(attemptID, terminal); err == nil {
		t.Fatal("second terminal append succeeded, want duplicate rejection")
	}

	for _, table := range []string{"invocation_attempt_starts", "invocation_attempt_terminals"} {
		for _, forbidden := range []string{"prompt", "cwd", "schema", "output", "reasoning", "error", "credential", "environment", "arguments"} {
			if hasColumn(t, d, table, forbidden) {
				t.Fatalf("%s contains forbidden raw-payload column %q", table, forbidden)
			}
		}
	}
}

func TestUtilityInvocationScopeDoesNotFabricatePipelineRows(t *testing.T) {
	d := openTestDB(t)
	utility, err := d.InsertUtilityScope(types.UtilityScopeWizard, os.Getpid())
	if err != nil {
		t.Fatalf("insert utility scope: %v", err)
	}
	attemptID, err := d.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      types.PurposeBranchCommitSuggestion,
		Role:         types.InvocationRoleFixer,
		Scope:        types.InvocationScope{Kind: types.InvocationScopeUtility, UtilityScopeID: utility.ID},
		CandidateKey: types.LegacyCandidateKey,
	})
	if err != nil {
		t.Fatalf("start utility invocation: %v", err)
	}
	attempt, err := d.GetInvocationAttempt(attemptID)
	if err != nil {
		t.Fatalf("get utility invocation: %v", err)
	}
	if attempt.Start.Scope.RunID != "" || attempt.Start.Scope.StepResultID != "" || attempt.Start.Scope.StepRoundID != "" {
		t.Fatalf("utility scope contains fabricated pipeline IDs: %+v", attempt.Start.Scope)
	}
	var runs, steps, rounds int
	if err := d.sql.QueryRow("SELECT (SELECT count(*) FROM runs), (SELECT count(*) FROM step_results), (SELECT count(*) FROM step_rounds)").Scan(&runs, &steps, &rounds); err != nil {
		t.Fatalf("count pipeline rows: %v", err)
	}
	if runs != 0 || steps != 0 || rounds != 0 {
		t.Fatalf("pipeline row counts = (%d, %d, %d), want all zero", runs, steps, rounds)
	}
}

func TestInvocationAttemptRejectsMismatchedPipelineOwnership(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/ownership", "git@github.com:user/ownership.git", "main")
	runA, _ := d.InsertRun(repo.ID, "feature-a", "aaa", "base")
	runB, _ := d.InsertRun(repo.ID, "feature-b", "bbb", "base")
	stepA, _ := d.InsertStepResult(runA.ID, types.StepReview)
	roundA, _ := d.ReserveStepRound(stepA.ID, 1, "initial")

	_, err := d.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      types.PurposeInitialReview,
		Role:         types.InvocationRoleVerifier,
		Scope:        types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: runB.ID, StepResultID: stepA.ID, StepRoundID: roundA.ID},
		CandidateKey: types.LegacyCandidateKey,
	})
	if err == nil {
		t.Fatal("mismatched run/step/round ownership was accepted")
	}
}

func TestRecoverStaleRunsInterruptsOpenInvocationAttempts(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/recovery", "git@github.com:user/recovery.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	_ = d.UpdateRunStatus(run.ID, types.RunRunning)
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	_ = d.StartStep(step.ID)
	round, _ := d.ReserveStepRound(step.ID, 1, "initial")
	attemptID, err := d.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      types.PurposeInitialReview,
		Role:         types.InvocationRoleVerifier,
		Scope:        types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: run.ID, StepResultID: step.ID, StepRoundID: round.ID},
		CandidateKey: types.LegacyCandidateKey,
	})
	if err != nil {
		t.Fatalf("start invocation: %v", err)
	}

	if _, err := d.RecoverStaleRuns("daemon crashed"); err != nil {
		t.Fatalf("recover stale runs: %v", err)
	}
	attempt, err := d.GetInvocationAttempt(attemptID)
	if err != nil {
		t.Fatalf("get recovered invocation: %v", err)
	}
	if attempt.Terminal == nil || attempt.Terminal.Outcome != types.InvocationOutcomeInterrupted {
		t.Fatalf("recovered terminal = %+v, want interrupted", attempt.Terminal)
	}
	if err := d.FinishInvocationAttempt(attemptID, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded}); err == nil {
		t.Fatal("completed recovered invocation accepted a second terminal")
	}
}

func TestRecoverStaleRunsLeavesLiveUtilityAttemptOpen(t *testing.T) {
	d := openTestDB(t)
	utility, err := d.InsertUtilityScope(types.UtilityScopeWizard, os.Getpid())
	if err != nil {
		t.Fatalf("insert utility scope: %v", err)
	}
	attemptID, err := d.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      types.PurposeBranchCommitSuggestion,
		Role:         types.InvocationRoleFixer,
		Scope:        types.InvocationScope{Kind: types.InvocationScopeUtility, UtilityScopeID: utility.ID},
		CandidateKey: types.LegacyCandidateKey,
	})
	if err != nil {
		t.Fatalf("start utility invocation: %v", err)
	}
	if _, err := d.RecoverStaleRuns("daemon restarted"); err != nil {
		t.Fatalf("recover stale runs: %v", err)
	}
	attempt, err := d.GetInvocationAttempt(attemptID)
	if err != nil {
		t.Fatalf("get utility invocation: %v", err)
	}
	if attempt == nil || attempt.Terminal != nil {
		t.Fatalf("utility invocation after pipeline recovery = %+v, want still active", attempt)
	}
}

func TestInvocationFactsSurvivePipelineOwnerDeletion(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/retention", "git@github.com:user/retention.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	round, _ := d.ReserveStepRound(step.ID, 1, "initial")
	attemptID, err := d.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      types.PurposeInitialReview,
		Role:         types.InvocationRoleVerifier,
		Scope:        types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: run.ID, StepResultID: step.ID, StepRoundID: round.ID},
		CandidateKey: types.LegacyCandidateKey,
	})
	if err != nil {
		t.Fatalf("start invocation: %v", err)
	}
	if err := d.FinishInvocationAttempt(attemptID, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded}); err != nil {
		t.Fatalf("finish invocation: %v", err)
	}
	if err := d.DeleteRepo(repo.ID); err != nil {
		t.Fatalf("delete pipeline owner: %v", err)
	}
	attempt, err := d.GetInvocationAttempt(attemptID)
	if err != nil {
		t.Fatalf("get retained invocation: %v", err)
	}
	if attempt == nil || attempt.Terminal == nil || attempt.Terminal.Outcome != types.InvocationOutcomeSucceeded {
		t.Fatalf("retained invocation = %+v, want immutable completed facts", attempt)
	}
}

func TestRecoverStaleRunsInterruptsOpenAttemptAfterOwnerDeletion(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/orphaned-attempt", "git@github.com:user/orphaned-attempt.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	round, _ := d.ReserveStepRound(step.ID, 1, "initial")
	attemptID, err := d.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      types.PurposeInitialReview,
		Role:         types.InvocationRoleVerifier,
		Scope:        types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: run.ID, StepResultID: step.ID, StepRoundID: round.ID},
		CandidateKey: types.LegacyCandidateKey,
	})
	if err != nil {
		t.Fatalf("start invocation: %v", err)
	}
	if err := d.DeleteRepo(repo.ID); err != nil {
		t.Fatalf("delete pipeline owner: %v", err)
	}
	if _, err := d.RecoverStaleRuns("daemon restarted"); err != nil {
		t.Fatalf("recover stale runs: %v", err)
	}
	attempt, err := d.GetInvocationAttempt(attemptID)
	if err != nil {
		t.Fatalf("get recovered invocation: %v", err)
	}
	if attempt == nil || attempt.Terminal == nil || attempt.Terminal.Outcome != types.InvocationOutcomeInterrupted {
		t.Fatalf("orphaned pipeline attempt = %+v, want interrupted", attempt)
	}
}

func TestGetRecentUtilityScopesFiltersAndLimits(t *testing.T) {
	d := openTestDB(t)
	for i := 0; i < 3; i++ {
		if _, err := d.InsertUtilityScope(types.UtilityScopeWizard, os.Getpid()); err != nil {
			t.Fatalf("insert utility scope %d: %v", i, err)
		}
	}
	all, err := d.GetRecentUtilityScopes(types.UtilityScopeWizard, 0)
	if err != nil {
		t.Fatalf("get recent (default limit): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("recent wizard scopes = %d, want 3", len(all))
	}
	for _, sc := range all {
		if sc.Kind != types.UtilityScopeWizard {
			t.Fatalf("scope kind = %q, want wizard", sc.Kind)
		}
	}
	limited, err := d.GetRecentUtilityScopes(types.UtilityScopeWizard, 2)
	if err != nil {
		t.Fatalf("get recent (limit 2): %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("limited wizard scopes = %d, want 2", len(limited))
	}
}
