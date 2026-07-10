package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func startRoutedReviewAttempt(t *testing.T, d *DB, run *Run, step *StepResult, round *StepRound) string {
	t.Helper()
	attemptID, err := d.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      types.PurposeInitialReview,
		Role:         types.InvocationRoleVerifier,
		Scope:        types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: run.ID, StepResultID: step.ID, StepRoundID: round.ID},
		CandidateKey: "review_strong:0:codex",
		Candidate: types.InvocationCandidate{
			Profile:        "review_strong",
			Tier:           0,
			CandidateIndex: 0,
			Runner:         types.RunnerCodex,
			Model:          "gpt-5.6-sol",
			Effort:         types.EffortHigh,
		},
	})
	if err != nil {
		t.Fatalf("start routed review attempt: %v", err)
	}
	return attemptID
}

func TestStartInvocationAttemptPersistsRoutedCandidate(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/routed", "git@github.com:user/routed.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	round, _ := d.ReserveStepRound(step.ID, 1, "initial")

	attemptID := startRoutedReviewAttempt(t, d, run, step, round)
	if err := d.FinishInvocationAttempt(attemptID, types.InvocationAttemptTerminal{
		Outcome:      types.InvocationOutcomeSucceeded,
		DurationMS:   4200,
		InputTokens:  120,
		OutputTokens: 34,
	}); err != nil {
		t.Fatalf("finish attempt: %v", err)
	}

	attempt, err := d.GetInvocationAttempt(attemptID)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	want := types.InvocationCandidate{Profile: "review_strong", Tier: 0, CandidateIndex: 0, Runner: types.RunnerCodex, Model: "gpt-5.6-sol", Effort: types.EffortHigh}
	if attempt.Start.Candidate != want {
		t.Fatalf("candidate = %+v, want %+v", attempt.Start.Candidate, want)
	}
	if attempt.Terminal == nil || attempt.Terminal.Outcome != types.InvocationOutcomeSucceeded {
		t.Fatalf("terminal = %+v, want succeeded", attempt.Terminal)
	}
}

func TestLegacyAttemptHasZeroCandidate(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/legacy-cand", "git@github.com:user/legacy-cand.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepTest)
	round, _ := d.ReserveStepRound(step.ID, 1, "initial")
	attemptID, err := d.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      types.PurposeTestEvidence,
		Role:         types.InvocationRoleFixer,
		Scope:        types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: run.ID, StepResultID: step.ID, StepRoundID: round.ID},
		CandidateKey: types.LegacyCandidateKey,
	})
	if err != nil {
		t.Fatalf("start legacy attempt: %v", err)
	}
	attempt, err := d.GetInvocationAttempt(attemptID)
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	if !attempt.Start.Candidate.IsZero() {
		t.Fatalf("legacy attempt candidate = %+v, want zero", attempt.Start.Candidate)
	}
}

func TestCreateFindingLineagesAreRunWideAndStable(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/lineage", "git@github.com:user/lineage.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	round, _ := d.ReserveStepRound(step.ID, 1, "initial")
	attemptID := startRoutedReviewAttempt(t, d, run, step, round)

	first, err := d.CreateFindingLineages(run.ID, attemptID, []string{"review-1", "review-2"})
	if err != nil {
		t.Fatalf("create lineages: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("created %d lineages, want 2", len(first))
	}
	if first[0].ID == "" || first[0].ID == first[0].DisplayID {
		t.Fatalf("lineage ID must be a fresh identity independent of the display id: %+v", first[0])
	}
	if first[0].Sequence != 0 || first[1].Sequence != 1 {
		t.Fatalf("sequences = %d,%d, want 0,1", first[0].Sequence, first[1].Sequence)
	}

	// A second batch (e.g. a later attempt reusing display id "review-1")
	// continues the run-wide monotonic sequence and mints distinct identities.
	second, err := d.CreateFindingLineages(run.ID, attemptID, []string{"review-1"})
	if err != nil {
		t.Fatalf("create lineages again: %v", err)
	}
	if second[0].Sequence != 2 {
		t.Fatalf("second batch sequence = %d, want 2", second[0].Sequence)
	}
	if second[0].ID == first[0].ID {
		t.Fatal("a reused display id must still receive a distinct root lineage")
	}

	byRun, err := d.GetFindingLineagesByRun(run.ID)
	if err != nil {
		t.Fatalf("get by run: %v", err)
	}
	if len(byRun) != 3 {
		t.Fatalf("run lineages = %d, want 3", len(byRun))
	}
	byAttempt, err := d.GetFindingLineagesByAttempt(attemptID)
	if err != nil {
		t.Fatalf("get by attempt: %v", err)
	}
	if len(byAttempt) != 3 {
		t.Fatalf("attempt lineages = %d, want 3", len(byAttempt))
	}
}

func TestReviewRoutingForStepProjectsActiveThenCompleted(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/proj", "git@github.com:user/proj.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	round, _ := d.ReserveStepRound(step.ID, 1, "initial")

	// Before any attempt, no routing projection exists.
	if proj, err := d.ReviewRoutingForStep(step.ID); err != nil || proj != nil {
		t.Fatalf("empty projection = %+v, %v; want nil", proj, err)
	}

	attemptID := startRoutedReviewAttempt(t, d, run, step, round)
	// Active: attempt started, no terminal, round still reserved.
	proj, err := d.ReviewRoutingForStep(step.ID)
	if err != nil {
		t.Fatalf("active projection: %v", err)
	}
	if proj == nil || len(proj.Attempts) != 1 || proj.Attempts[0].Terminal != nil {
		t.Fatalf("active projection = %+v, want one in-flight attempt", proj)
	}
	if proj.Attempts[0].Start.Candidate.Model != "gpt-5.6-sol" {
		t.Fatalf("projected model = %q", proj.Attempts[0].Start.Candidate.Model)
	}

	_ = d.FinishInvocationAttempt(attemptID, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded, DurationMS: 10})
	_, _ = d.CreateFindingLineages(run.ID, attemptID, []string{"review-1"})
	_ = d.CompleteReservedStepRound(round.ID, nil, nil, 10)

	proj, err = d.ReviewRoutingForStep(step.ID)
	if err != nil {
		t.Fatalf("completed projection: %v", err)
	}
	if proj == nil || len(proj.Attempts) != 1 || proj.Attempts[0].Terminal == nil {
		t.Fatalf("completed projection attempts = %+v, want one finalized attempt", proj)
	}
	if len(proj.Lineages) != 1 {
		t.Fatalf("completed projection lineages = %d, want 1", len(proj.Lineages))
	}
}
