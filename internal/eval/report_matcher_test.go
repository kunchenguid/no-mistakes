package eval

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Report must keep saved ID-only scores separate from scores using the
// file-and-line matcher, while aggregating repeated measurements of that
// matcher. The legacy cohort fixture is SHA-256("1\x00matcher-version")[:8],
// the historical cohort contract for one repeat of this case. Reusing that
// cohort for the stricter matcher would combine the collision's false credit
// with its corrected miss in a single report.
func TestReportSeparatesMatcherRevisions(t *testing.T) {
	store := openEvalStore(t)
	c := writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "matcher-version", fingerprint: "repo-a", capturedAt: 1,
		gold: []FindingGold{{ID: "review-1", Kind: GoldTruePositive,
			File: "main.go", Line: 10, Description: "overflow"}},
	})
	candidate := Candidate{Agent: types.AgentClaude, Model: "test"}
	legacy := Evaluation{
		ID: "legacy", SessionID: "legacy", CaseID: c.ID, Candidate: candidate.String(),
		Cohort: "9b4fa04ddf48a92a", Repeat: 1, Status: "completed",
		HasFindingGold: true, GoldCount: 1, TruePositive: 1, TruePositiveExact: 1,
		FindingsJSON: mustFindingJSON(t, "review-1", "other.go", 10, "deadlock"),
	}
	if err := store.persistEvaluation(c, legacy); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, session, err := store.prepareReplay(context.Background(), ReplayOptions{
			Set: "labeled", Candidate: candidate, Repeats: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer store.releaseReplayReservation(session.ID)
		current := legacy
		current.ID = session.ID
		current.SessionID = session.ID
		current.Cohort = session.Cohort
		current.TruePositive = 0
		current.TruePositiveExact = 0
		current.FalseNegative = 1
		current.Pending = 1
		if err := store.persistEvaluation(c, current); err != nil {
			t.Fatal(err)
		}
	}

	reports, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %#v, want separate historical and current matcher cohorts", reports)
	}
	for _, report := range reports {
		summary := report.Summary
		if summary.Candidate != candidate.String() || report.RepeatCount != 1 {
			t.Fatalf("report identity = %#v, want the same candidate and repeat plan", report)
		}
		if report.Cohort == legacy.Cohort {
			if summary.Total != 1 || summary.TruePositive != 1 || summary.TruePositiveExact != 1 || summary.FalseNegative != 0 || summary.Pending != 0 {
				t.Fatalf("historical scores = %#v, want the single saved ID-only match", summary)
			}
		} else if summary.Total != 2 || summary.TruePositive != 0 || summary.TruePositiveExact != 0 || summary.FalseNegative != 2 || summary.Pending != 2 {
			t.Fatalf("current scores = %#v, want two corrected misses in one cohort", summary)
		}
	}
}
