package daemon

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestStepToInfoIncludesFixSummaries(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc", "def")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}

	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"x"}],"summary":"1"}`
	if _, err := d.InsertStepRound(step.ID, 1, "initial", &findings, nil, 100); err != nil {
		t.Fatalf("insert round 1: %v", err)
	}
	sum := "handle nil pointer in executor"
	if _, err := d.InsertStepRound(step.ID, 2, "auto_fix", nil, &sum, 100); err != nil {
		t.Fatalf("insert round 2: %v", err)
	}

	info := stepToInfo(d, step)
	if len(info.FixSummaries) != 1 || info.FixSummaries[0] != sum {
		t.Errorf("fix summaries = %v, want [%q]", info.FixSummaries, sum)
	}
}

func TestStepToInfoNoFixSummariesWithoutFixRounds(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc", "def")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepLint)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if _, err := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 100); err != nil {
		t.Fatalf("insert round: %v", err)
	}

	info := stepToInfo(d, step)
	if len(info.FixSummaries) != 0 {
		t.Errorf("fix summaries = %v, want none", info.FixSummaries)
	}
}

func TestStepToInfoIncludesReviewRouting(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc", "def")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	round, err := d.ReserveStepRound(step.ID, 1, "initial")
	if err != nil {
		t.Fatalf("reserve round: %v", err)
	}

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
		t.Fatalf("start attempt: %v", err)
	}
	if err := d.FinishInvocationAttempt(attemptID, types.InvocationAttemptTerminal{
		Outcome:      types.InvocationOutcomeSucceeded,
		DurationMS:   4200,
		InputTokens:  120,
		OutputTokens: 34,
	}); err != nil {
		t.Fatalf("finish attempt: %v", err)
	}
	if _, err := d.CreateFindingLineages(run.ID, attemptID, []string{"review-1", "review-2"}); err != nil {
		t.Fatalf("create lineages: %v", err)
	}

	info := stepToInfo(d, step)
	if info.ReviewRouting == nil {
		t.Fatal("ReviewRouting = nil, want populated for a routed review step")
	}
	if len(info.ReviewRouting.Candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(info.ReviewRouting.Candidates))
	}
	c := info.ReviewRouting.Candidates[0]
	if c.Profile != "review_strong" || c.Runner != "codex" || c.Model != "gpt-5.6-sol" || c.Effort != "high" {
		t.Errorf("routed candidate facts = %+v", c)
	}
	if c.Outcome != "succeeded" {
		t.Errorf("outcome = %q, want succeeded", c.Outcome)
	}
	if c.DurationMS != 4200 || c.InputTokens != 120 || c.OutputTokens != 34 {
		t.Errorf("terminal facts = %+v", c)
	}
	if info.ReviewRouting.LineageCount != 2 {
		t.Errorf("lineage count = %d, want 2", info.ReviewRouting.LineageCount)
	}
}

func TestStepToInfoNilReviewRoutingForLegacyReview(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc", "def")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	round, err := d.ReserveStepRound(step.ID, 1, "initial")
	if err != nil {
		t.Fatalf("reserve round: %v", err)
	}
	// A legacy, unrouted attempt (zero Candidate) records no routing, so the
	// projection filter excludes it and stepToInfo leaves ReviewRouting nil.
	if _, err := d.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      types.PurposeInitialReview,
		Role:         types.InvocationRoleVerifier,
		Scope:        types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: run.ID, StepResultID: step.ID, StepRoundID: round.ID},
		CandidateKey: types.LegacyCandidateKey,
	}); err != nil {
		t.Fatalf("start legacy attempt: %v", err)
	}

	info := stepToInfo(d, step)
	if info.ReviewRouting != nil {
		t.Errorf("ReviewRouting = %+v, want nil for legacy review", info.ReviewRouting)
	}
}
