package eval

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The sets inspection carries an instant self-score: the recorded source
// reviews scored against their own gold with the same matcher a replayed
// candidate faces. No agent or replay is involved, so it must be computable
// from the frozen case files alone.
func TestInspectSetsSelfScoresRecordedReviewsAgainstTheirGold(t *testing.T) {
	store := openEvalStore(t)
	// The recorded review raised "hit" (fixed by the user: TP gold) and
	// "noise" (shipped unfixed: FP gold), and missed "missed" (user-added FN
	// gold).
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "scored", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
		changedFiles: []string{"main.go"},
		gold: []FindingGold{
			{ID: "hit", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "real bug", Severity: "error", Action: "auto-fix"},
			{ID: "missed", Kind: GoldFalseNegative, Source: goldSourceUserAdded, File: "main.go", Line: 9, Description: "missing audit", Severity: "warning", Action: "auto-fix"},
			{ID: "noise", Kind: GoldFalsePositive, Source: goldSourceShippedUnfixed, File: "main.go", Line: 5, Description: "style nit", Severity: "info", Action: "ask-user"},
		},
		roundFindings: findingsJSON(
			findingSpec{ID: "hit", Severity: "error", File: "main.go", Line: 1, Description: "real bug", Action: "auto-fix"},
			findingSpec{ID: "noise", Severity: "info", File: "main.go", Line: 5, Description: "style nit", Action: "ask-user"},
		),
	})

	diversified := mustSetSummary(t, store, "diversified")
	score := diversified.SelfScore
	if score.Labeled != 1 {
		t.Fatalf("self-score labeled = %d, want 1", score.Labeled)
	}
	if score.TruePositive != 1 || score.FalseNegative != 1 || score.FalsePositive != 1 || score.Pending != 0 {
		t.Fatalf("self-score = %#v, want TP 1 / FN 1 / FP 1 / pending 0", score)
	}
	if got := score.Recall(); got != 0.5 {
		t.Fatalf("self-score recall = %v, want 0.5", got)
	}
	if !score.HasFalsePositiveGold() {
		t.Fatalf("self-score = %#v, want false-positive gold present so F1 is headline-eligible", score)
	}
}

func TestSelfScoreRecordedReviewsLeavesUnlabeledCasesPending(t *testing.T) {
	store := openEvalStore(t)
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "unlabeled", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
		changedFiles:  []string{"main.go"},
		roundFindings: findingsJSON(findingSpec{ID: "f1", Severity: "error", File: "main.go", Line: 3, Description: "bug", Action: "ask-user"}),
	})

	all := mustSetSummary(t, store, "all")
	if all.SelfScore.Labeled != 0 {
		t.Fatalf("self-score labeled = %d, want 0 for a gold-less corpus", all.SelfScore.Labeled)
	}
	if all.SelfScore.Pending != 1 {
		t.Fatalf("self-score pending = %d, want the recorded finding queued, not scored", all.SelfScore.Pending)
	}
}

// Replay surfaces its plan and per-result progress synchronously so an
// interactive caller can stream the session as it happens.
func TestReplayReportsPlanAndPerResultProgress(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	installFakeReviewAgent(t, p, `{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`)

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err != nil {
		t.Fatal(err)
	}

	var plannedCases int
	var plannedCohort string
	type progress struct{ completed, total int }
	var results []progress
	_, evaluations, err := Replay(ctx, store, ReplayOptions{
		Set:       "labeled",
		Candidate: Candidate{Agent: types.AgentClaude, Model: "test"},
		Repeats:   2,
		OnPlan: func(session Session, cases []Case) {
			plannedCases = len(cases)
			plannedCohort = session.Cohort
		},
		OnResult: func(evaluation Evaluation, completed, total int) {
			results = append(results, progress{completed, total})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plannedCases != 1 || plannedCohort == "" {
		t.Fatalf("plan callback saw %d case(s), cohort %q; want 1 case and a cohort", plannedCases, plannedCohort)
	}
	if len(evaluations) != 2 || len(results) != 2 {
		t.Fatalf("progress callbacks = %#v over %d evaluations, want one per replay", results, len(evaluations))
	}
	for i, got := range results {
		if got.completed != i+1 || got.total != 2 {
			t.Fatalf("progress %d = %+v, want completed %d of total 2", i, got, i+1)
		}
	}
}
