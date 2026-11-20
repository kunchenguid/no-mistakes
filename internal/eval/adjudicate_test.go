package eval

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

func TestAdjudicationReportsRescoreSavedOutputsOnlyAfterExplicitDecision(t *testing.T) {
	ctx := context.Background()
	p, database, run, _, round := setupCapturedRun(t, ctx)
	defer database.Close()
	if err := database.SetStepRoundSelection(round.ID, strPtr(`[]`), db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRState(run.ID, "merged"); err != nil {
		t.Fatal(err)
	}
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, database, run.ID)
	if err != nil || len(cases) != 1 {
		t.Fatalf("capture: %v %v", cases, err)
	}
	c := cases[0]
	if c.Labels.Findings[0].Source != goldSourceShippedUnfixed {
		t.Fatalf("expected inferred FP: %#v", c.Labels)
	}
	for _, row := range []Evaluation{
		{ID: "one", SessionID: "session", CaseID: c.ID, Candidate: "claude,model=sonnet", Cohort: "cohort", Repeat: 1, Status: "completed", HasFindingGold: true, FalsePositiveGold: 1, FalsePositive: 1, FindingsJSON: *round.FindingsJSON},
		{ID: "two", SessionID: "session", CaseID: c.ID, Candidate: "claude,model=sonnet", Cohort: "cohort", Repeat: 2, Status: "completed", HasFindingGold: true, FalsePositiveGold: 1, FalsePositive: 1, FindingsJSON: *round.FindingsJSON},
		{ID: "failed", SessionID: "session", CaseID: c.ID, Candidate: "claude,model=sonnet", Cohort: "cohort", Repeat: 3, Status: "failed", HasFindingGold: true, FalsePositiveGold: 1, Error: "provider failed"},
	} {
		if err := store.persistEvaluation(c, row); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := store.evaluations()
	if err != nil {
		t.Fatal(err)
	}
	report := func() CandidateReport {
		t.Helper()
		reports, err := Report(store)
		if err != nil || len(reports) != 1 {
			t.Fatalf("report: %v %v", reports, err)
		}
		return reports[0]
	}
	assertQueued := func(want int) {
		t.Helper()
		sets, err := InspectSets(store)
		if err != nil {
			t.Fatal(err)
		}
		if len(sets) != 4 {
			t.Fatalf("sets: %#v", sets)
		}
		for _, set := range sets {
			expected := want
			if set.Cases == 0 {
				expected = 0
			}
			if set.QueuedFindings != expected {
				t.Fatalf("set %s queued = %d, want %d", set.Name, set.QueuedFindings, expected)
			}
		}
	}
	baseline := report()
	assertQueued(0)
	if baseline.Summary.FalsePositive != 2 || baseline.Summary.FalsePositiveGold != 3 || baseline.Provenance.Inferred != 1 || baseline.AdjudicatedCases != 0 {
		t.Fatalf("baseline: %#v", baseline)
	}
	if text := RenderReport([]CandidateReport{baseline}); strings.Contains(text, "(adjudicated ") || !strings.Contains(text, "inferred 1") {
		t.Fatal(text)
	}
	_, err = store.Adjudicate(ctx, c.ID, "real-bug", 0, Adjudication{Kind: GoldTruePositive, Reason: "real defect accepted as risk"})
	if err != nil {
		t.Fatal(err)
	}
	corrected := report()
	assertQueued(0)
	if corrected.Summary.TruePositive != 2 || corrected.Summary.FalseNegative != 1 || corrected.Summary.FalsePositive != 0 || corrected.Summary.Failures != 1 || corrected.Provenance.Adjudicated != 1 || corrected.AdjudicatedCases != 1 {
		t.Fatalf("corrected: %#v", corrected)
	}
	if text := RenderReport([]CandidateReport{corrected}); !strings.Contains(text, "explicitly adjudicated case(s)") || !strings.Contains(text, "F1: withheld") {
		t.Fatal(text)
	}
	_, err = store.Adjudicate(ctx, c.ID, "real-bug", 0, Adjudication{Excluded: true, Reason: "disputed gold"})
	if err != nil {
		t.Fatal(err)
	}
	excluded := report()
	assertQueued(2)
	if excluded.Summary.Labeled != 0 || excluded.Summary.Pending != 2 || excluded.Summary.TruePositive != 0 || excluded.Provenance.Excluded != 1 {
		t.Fatalf("excluded: %#v", excluded)
	}
	for _, set := range []string{"labeled", "diversified", "tune"} {
		if cases, err := store.ListCases(set); err != nil || len(cases) != 0 {
			t.Fatalf("set %s: %v %v", set, cases, err)
		}
	}
	if cases, err := store.ListCases("all"); err != nil || len(cases) != 1 {
		t.Fatalf("excluded case lost: %v %v", cases, err)
	}
	_, err = store.Adjudicate(ctx, c.ID, "real-bug", 0, Adjudication{Kind: GoldFalsePositive, Reason: "confirmed false alarm"})
	if err != nil {
		t.Fatal(err)
	}
	if restored := report(); restored.Summary.Pending != 0 || restored.Summary.FalsePositive != 2 {
		t.Fatalf("restored: %#v", restored)
	}
	assertQueued(0)
	after, err := store.evaluations()
	if err != nil || !reflect.DeepEqual(stored, after) {
		t.Fatalf("report changed saved evaluations: %v", err)
	}
	// Corrupt decisions must not silently fall back to old scores.
	bad, err := store.CaseByID(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	bad.Labels.Findings[0].Adjudication.Kind = "invented"
	bad.Labels.Findings[0].Adjudication.Excluded = false
	if err := writeJSON(filepath.Join(c.Dir, "labels.json"), bad.Labels); err != nil {
		t.Fatal(err)
	}
	if _, err := Report(store); err == nil {
		t.Fatal("report accepted invalid adjudication")
	}
}

func TestAdjudicationIndexAndAmbiguousIDs(t *testing.T) {
	ctx := context.Background()
	p, database, run, _, _ := setupCapturedRun(t, ctx)
	defer database.Close()
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := cases[0]
	c.Labels.Findings = []FindingGold{{Kind: GoldTruePositive, Description: "legacy id-less gold"}, {ID: "duplicate", Kind: GoldFalsePositive}, {ID: "duplicate", Kind: GoldTruePositive}}
	if err := writeJSON(filepath.Join(c.Dir, "labels.json"), c.Labels); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(c.Dir, "labels.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Adjudicate(ctx, c.ID, "duplicate", 0, Adjudication{Excluded: true, Reason: "ambiguous"}); err == nil {
		t.Fatal("accepted ambiguous ID")
	}
	after, err := os.ReadFile(filepath.Join(c.Dir, "labels.json"))
	if err != nil || string(before) != string(after) {
		t.Fatalf("ambiguous selection mutated labels: %v", err)
	}
	updated, err := store.Adjudicate(ctx, c.ID, "", 1, Adjudication{Kind: GoldFalseNegative, Reason: "confirmed miss"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Labels.Findings[0].ID != "" || updated.Labels.Findings[0].Kind != GoldTruePositive || updated.Labels.ActiveFindings()[0].Kind != GoldFalseNegative {
		t.Fatalf("id-less correction: %#v", updated.Labels)
	}
	updated, err = store.Adjudicate(ctx, c.ID, "", 3, Adjudication{Excluded: true, Reason: "exclude only the second duplicate"})
	if err != nil {
		t.Fatal(err)
	}
	merged := mergeGold(updated.Labels, Labels{Version: labelsVersion, Findings: []FindingGold{{ID: "duplicate", Kind: GoldFalsePositive, Source: goldSourceShippedUnfixed}}})
	var decisions int
	for _, gold := range merged.Findings {
		if gold.Adjudication != nil {
			decisions++
		}
	}
	if decisions != 2 {
		t.Fatalf("refresh lost explicit decisions: %#v", merged)
	}
}

func TestGoldProvenanceBackwardCompatibility(t *testing.T) {
	for source, want := range map[string]string{
		goldSourceShippedUnfixed: "inferred", goldSourceAutoFixMerged: "inferred", goldSourceCIFalseNegative: "inferred",
		goldSourceUserFix: "recorded-human", goldSourceUserAdded: "recorded-human", goldSourcePostPRMiss: "recorded-human",
		"": "unknown", "old-curator": "unknown",
	} {
		g := FindingGold{ID: "f", Kind: GoldFalsePositive, Source: source}
		if got := g.GoldProvenance(); got != want {
			t.Fatalf("%q provenance %q", source, got)
		}
		labels := Labels{Version: labelsVersion, Findings: []FindingGold{g}}
		if err := validateLabels(labels); err != nil {
			t.Fatal(err)
		}
		if score := ScoreCandidate(labels, `{"findings":[{"id":"f"}]}`); score.FalsePositive != 1 {
			t.Fatalf("legacy scoring changed: %#v", score)
		}
	}
}

func TestAppendingMissGoldPreservesAnInterveningAdjudication(t *testing.T) {
	ctx := context.Background()
	p, database, run, _, _ := setupCapturedRun(t, ctx)
	defer database.Close()
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, database, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale := cases[0] // miss ingest selects its target before acquiring the lock
	if _, err := store.Adjudicate(ctx, stale.ID, "real-bug", 0, Adjudication{Excluded: true, Reason: "disputed"}); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockCorpus(ctx, store.root)
	if err != nil {
		t.Fatal(err)
	}
	_, added, err := store.appendFindingGold(stale, []FindingGold{{ID: "later-miss", Kind: GoldFalseNegative, Description: "confirmed later miss"}})
	unlock()
	if err != nil || added != 1 {
		t.Fatalf("append: %d %v", added, err)
	}
	current, err := store.CaseByID(stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Labels.Findings) != 2 || current.Labels.Findings[0].Adjudication == nil || !current.Labels.Findings[0].Adjudication.Excluded || current.Labels.TrueIssueCount() != 1 {
		t.Fatalf("append overwrote decision: %#v", current.Labels)
	}
}
