package eval

import (
	"strings"
	"testing"
)

func TestScoreCandidateMatchesNearbyLineWithSimilarDescription(t *testing.T) {
	labels := Labels{Findings: []FindingGold{{
		ID:          "gold",
		Kind:        GoldTruePositive,
		File:        "internal/eval/score.go",
		Line:        10,
		Description: "drops an HTTP error on the handler path",
	}}}
	candidate := `{"findings":[{"id":"other","file":"internal/eval/score.go","line":12,"description":"drops an HTTP error on handler path"}]}`

	score := ScoreCandidate(labels, candidate)
	if score.TruePositive != 1 || score.TruePositiveFuzzy != 1 || score.TruePositiveExact != 0 || score.FalseNegative != 0 || score.Pending != 0 {
		t.Fatalf("score = %#v, want a fuzzy location match", score)
	}
}

func TestScoreCandidateDoesNotMatchShortContainment(t *testing.T) {
	labels := Labels{Findings: []FindingGold{{
		ID:          "gold",
		Kind:        GoldTruePositive,
		File:        "main.go",
		Description: "bug in the widget factory initialization sequence during startup",
	}}}
	candidate := `{"findings":[{"id":"other","file":"main.go","description":"bug"}]}`

	score := ScoreCandidate(labels, candidate)
	if score.TruePositive != 0 || score.FalseNegative != 1 || score.Pending != 1 {
		t.Fatalf("score = %#v, want short containment left unmatched", score)
	}
}

func TestScoreCandidatePrefersExactOverFuzzy(t *testing.T) {
	labels := Labels{Findings: []FindingGold{{
		ID:          "error-handling",
		Kind:        GoldTruePositive,
		File:        "old.go",
		Line:        4,
		Description: "drops an HTTP error",
	}}}
	candidate := `{"findings":[{"id":"nearby","file":"old.go","line":5,"description":"drops an HTTP error on the handler"},{"id":"error-handling","file":"new.go","description":"unrelated"}]}`

	score := ScoreCandidate(labels, candidate)
	if score.TruePositive != 1 || score.TruePositiveExact != 1 || score.TruePositiveFuzzy != 0 || score.Pending != 1 {
		t.Fatalf("score = %#v, want exact-id to win over a nearby fuzzy candidate", score)
	}
}

func TestScoreCandidateDoesNotLetFuzzyEarlierGoldStealExactLaterMatch(t *testing.T) {
	labels := Labels{Findings: []FindingGold{
		{
			ID:          "nil-deref",
			Kind:        GoldTruePositive,
			File:        "main.go",
			Line:        10,
			Description: "nil pointer dereference in the request handler",
		},
		{
			ID:          "missing-unlock",
			Kind:        GoldTruePositive,
			File:        "lock.go",
			Line:        1,
			Description: "mutex not released on the error path",
		},
	}}
	candidate := `{"findings":[` +
		`{"id":"missing-unlock","file":"main.go","line":12,"description":"nil pointer deref in request handler"},` +
		`{"id":"other","file":"main.go","line":11,"description":"nil pointer dereference in the request handler during shutdown"}` +
		`]}`

	score := ScoreCandidate(labels, candidate)
	if score.TruePositive != 2 || score.FalseNegative != 0 {
		t.Fatalf("score = %#v, want both gold items matched (exact-id later gold plus leftover fuzzy cover for the earlier gold), not a greedy first-gold steal", score)
	}
}

func TestScoreCandidateKeepsUnmatchedPendingUntilAdjudicated(t *testing.T) {
	labels := Labels{Findings: []FindingGold{{
		ID:          "gold",
		Kind:        GoldTruePositive,
		File:        "main.go",
		Description: "real bug",
	}}}
	candidate := `{"findings":[{"id":"gold","file":"main.go","description":"real bug"},{"id":"extra","file":"main.go","description":"new later issue"}]}`

	score := ScoreCandidate(labels, candidate)
	if score.TruePositive != 1 || score.FalsePositive != 0 || score.Pending != 1 {
		t.Fatalf("score = %#v, want unmatched extra queued as pending, not FP", score)
	}
}

func TestScoreCandidateCountsExplicitFalsePositiveGold(t *testing.T) {
	labels := Labels{Findings: []FindingGold{{
		ID:          "noise",
		Kind:        GoldFalsePositive,
		File:        "main.go",
		Description: "style nit",
	}}}
	candidate := `{"findings":[{"id":"noise","file":"main.go","description":"style nit"}]}`

	score := ScoreCandidate(labels, candidate)
	if score.FalsePositive != 1 || score.FalsePositiveGold != 1 || score.Pending != 0 || score.TruePositive != 0 {
		t.Fatalf("score = %#v, want explicit FP gold counted", score)
	}
}

func TestEvaluationSummaryWithholdsHeadlineF1WithoutFalsePositiveGold(t *testing.T) {
	summary := SummarizeEvaluations([]Evaluation{{
		Candidate: "claude+test", Status: "completed", HasFindingGold: true, GoldCount: 2,
		TruePositive: 2, Pending: 1,
	}})
	if summary.Recall() != 1 {
		t.Fatalf("recall = %v, want 1", summary.Recall())
	}
	if summary.HasFalsePositiveGold() {
		t.Fatal("summary reported FP gold when none existed")
	}
	output := RenderReport([]CandidateReport{{Cohort: "c", Summary: summary, RepeatCount: 1}})
	if !strings.Contains(output, "recall: 100.0%") {
		t.Fatalf("report = %q, want recall as the headline", output)
	}
	if !strings.Contains(output, "precision") || !strings.Contains(output, "pending") {
		t.Fatalf("report = %q, want precision bounds and pending", output)
	}
	if strings.Contains(output, "F1:") && !strings.Contains(output, "F1: withheld") {
		t.Fatalf("report = %q, want F1 withheld when there is no false-positive gold", output)
	}
}

func TestEvaluationSummaryHeadlinesF1WhenFalsePositiveGoldExists(t *testing.T) {
	summary := SummarizeEvaluations([]Evaluation{{
		Candidate: "claude+test", Status: "completed", HasFindingGold: true, GoldCount: 2,
		TruePositive: 2, FalsePositive: 1, FalsePositiveGold: 1,
	}})
	if !summary.HasFalsePositiveGold() {
		t.Fatal("summary missing FP gold")
	}
	if got := summary.Precision(); got != 2.0/3.0 {
		t.Fatalf("precision = %v, want 2/3", got)
	}
	output := RenderReport([]CandidateReport{{Cohort: "c", Summary: summary, RepeatCount: 1}})
	if !strings.Contains(output, "F1:") || strings.Contains(output, "F1: withheld") {
		t.Fatalf("report = %q, want headline F1 once false-positive gold exists", output)
	}
}

func TestEvaluationSummaryPrecisionBoundsTreatPendingAsWorstCase(t *testing.T) {
	summary := EvaluationSummary{TruePositive: 1, FalseNegative: 1, FalsePositive: 0, Pending: 1, Labeled: 1}
	if got := summary.Precision(); got != 1 {
		t.Fatalf("precision_adj = %v, want 1 (no adjudicated FP)", got)
	}
	if got := summary.PrecisionLower(); got != 0.5 {
		t.Fatalf("precision_lower = %v, want 0.5 (pending treated as FP)", got)
	}
}
