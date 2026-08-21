package eval

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type syntheticCaseSpec struct {
	id            string
	fingerprint   string
	capturedAt    int64
	changedLines  int
	changedFiles  []string
	gold          []FindingGold
	roundFindings string
}

func TestListCasesDiversified_GoldOnlyEmptyWarns(t *testing.T) {
	store := openEvalStore(t)
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "unlabeled-tiny-go", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
		changedFiles:  []string{"main.go"},
		roundFindings: findingsJSON(findingSpec{ID: "f1", Severity: "error", File: "main.go", Line: 3, Description: "bug", Action: "ask-user"}),
	})

	got, err := store.ListCases("diversified")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("diversified = %#v, want empty when the corpus has no gold", got)
	}
	all, err := store.ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("all = %d, want the unlabeled case retained outside diversified", len(all))
	}

	diversified := mustSetSummary(t, store, "diversified")
	if diversified.Cases != 0 {
		t.Fatalf("diversified summary = %#v, want an empty diversified set", diversified)
	}
	if !strings.Contains(diversified.Warning, "no labeled gold") {
		t.Fatalf("diversified warning = %q, want an empty-gold warning, not a silent unlabeled fill", diversified.Warning)
	}
}

func TestListCasesDiversified_PrefersGoldAndPins(t *testing.T) {
	store := openEvalStore(t)
	store.SetDiversifiedSize(1)
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "unlabeled-first", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
		changedFiles:  []string{"main.go"},
		roundFindings: findingsJSON(findingSpec{ID: "f1", Severity: "error", File: "main.go", Line: 3, Description: "bug", Action: "ask-user"}),
	})
	rich := writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "gold-later-rich", fingerprint: "repo-a", capturedAt: 3, changedLines: 10,
		changedFiles: []string{"main.go"},
		gold: []FindingGold{
			{ID: "a", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "one", Severity: "error", Action: "auto-fix"},
			{ID: "b", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 2, Description: "two", Severity: "error", Action: "auto-fix"},
		},
		roundFindings: findingsJSON(
			findingSpec{ID: "a", Severity: "error", File: "main.go", Line: 1, Description: "one", Action: "auto-fix"},
			findingSpec{ID: "b", Severity: "error", File: "main.go", Line: 2, Description: "two", Action: "auto-fix"},
		),
	})
	thin := writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "gold-earlier-thin", fingerprint: "repo-a", capturedAt: 2, changedLines: 10,
		changedFiles: []string{"main.go"},
		gold: []FindingGold{
			{ID: "c", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 3, Description: "three", Severity: "error", Action: "auto-fix"},
		},
		roundFindings: findingsJSON(findingSpec{ID: "c", Severity: "error", File: "main.go", Line: 3, Description: "three", Action: "auto-fix"}),
	})

	first, err := store.ListCases("diversified")
	if err != nil {
		t.Fatal(err)
	}
	if ids := caseIDs(first); len(ids) != 1 || ids[0] != rich.ID {
		t.Fatalf("diversified = %v, want the richer gold case (not unlabeled, not the thinner sibling)", ids)
	}

	richerStill := writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "gold-richest-later", fingerprint: "repo-a", capturedAt: 4, changedLines: 10,
		changedFiles: []string{"main.go"},
		gold: []FindingGold{
			{ID: "d", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "d", Severity: "error", Action: "auto-fix"},
			{ID: "e", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 2, Description: "e", Severity: "error", Action: "auto-fix"},
			{ID: "f", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 3, Description: "f", Severity: "error", Action: "auto-fix"},
		},
		roundFindings: findingsJSON(
			findingSpec{ID: "d", Severity: "error", File: "main.go", Line: 1, Description: "d", Action: "auto-fix"},
			findingSpec{ID: "e", Severity: "error", File: "main.go", Line: 2, Description: "e", Action: "auto-fix"},
			findingSpec{ID: "f", Severity: "error", File: "main.go", Line: 3, Description: "f", Action: "auto-fix"},
		),
	})
	pinned, err := store.ListCases("diversified")
	if err != nil {
		t.Fatal(err)
	}
	if ids := caseIDs(pinned); len(ids) != 1 || ids[0] != rich.ID {
		t.Fatalf("pinned diversified = %v, want the original pin (anti-churn), not %s", ids, richerStill.ID)
	}

	refreshed, err := store.RefreshDiversified()
	if err != nil {
		t.Fatal(err)
	}
	if ids := caseIDs(refreshed); len(ids) != 1 || ids[0] != richerStill.ID {
		t.Fatalf("refreshed diversified = %v, want the richest gold case after an explicit refresh", ids)
	}
	_ = thin
}

func TestListCasesDiversified_HamiltonCap(t *testing.T) {
	store := openEvalStore(t)
	store.SetDiversifiedSize(3)
	// Weights 5, 3, 1 across three strata. Hamilton of cap 3 yields 2, 1, 0.
	writeGoldStratum(t, store, "repo-heavy", "error", 5, 10)
	writeGoldStratum(t, store, "repo-mid", "warning", 3, 20)
	writeGoldStratum(t, store, "repo-light", "info", 1, 30)

	got, err := store.ListCases("diversified")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, c := range got {
		counts[c.RepoFingerprint]++
	}
	if counts["repo-heavy"] != 2 || counts["repo-mid"] != 1 || counts["repo-light"] != 0 || len(got) != 3 {
		t.Fatalf("hamilton allocation = %#v (n=%d), want 2/1/0 over the three strata", counts, len(got))
	}
	again, err := store.ListCases("diversified")
	if err != nil {
		t.Fatal(err)
	}
	if ids := caseIDs(again); strings.Join(ids, ",") != strings.Join(caseIDs(got), ",") {
		t.Fatalf("diversified reshuffled across reads: first %v then %v", caseIDs(got), ids)
	}
}

func TestListCasesDiversified_SeverityAndFindingTypeAxes(t *testing.T) {
	store := openEvalStore(t)
	store.SetDiversifiedSize(0)
	errorFix := writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "error-autofix", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
		changedFiles: []string{"main.go"},
		gold: []FindingGold{
			{ID: "e1", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "err", Severity: "error", Action: "auto-fix"},
		},
		roundFindings: findingsJSON(findingSpec{ID: "e1", Severity: "error", File: "main.go", Line: 1, Description: "err", Action: "auto-fix"}),
	})
	warnAsk := writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "warning-askuser", fingerprint: "repo-a", capturedAt: 2, changedLines: 10,
		changedFiles: []string{"main.go"},
		gold: []FindingGold{
			{ID: "w1", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 2, Description: "warn", Severity: "warning", Action: "ask-user"},
		},
		roundFindings: findingsJSON(findingSpec{ID: "w1", Severity: "warning", File: "main.go", Line: 2, Description: "warn", Action: "ask-user"}),
	})
	fpOnly := writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "fp-only", fingerprint: "repo-a", capturedAt: 3, changedLines: 10,
		changedFiles: []string{"main.go"},
		gold: []FindingGold{
			{ID: "p1", Kind: GoldFalsePositive, Source: goldSourceShippedUnfixed, File: "main.go", Line: 3, Description: "noise", Severity: "info", Action: "ask-user"},
		},
		roundFindings: findingsJSON(findingSpec{ID: "p1", Severity: "info", File: "main.go", Line: 3, Description: "noise", Action: "ask-user"}),
	})

	got, err := store.ListCases("diversified")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, c := range got {
		ids[c.ID] = true
	}
	for _, want := range []string{errorFix.ID, warnAsk.ID, fpOnly.ID} {
		if !ids[want] {
			t.Fatalf("diversified = %v, want severity and finding-type to split %s into its own stratum", caseIDs(got), want)
		}
	}
}

func TestListCasesDiversified_LoweringCapTakesEffectWithoutRefresh(t *testing.T) {
	store := openEvalStore(t)
	store.SetDiversifiedSize(3)
	writeGoldStratum(t, store, "repo-a", "error", 1, 10)
	writeGoldStratum(t, store, "repo-b", "error", 1, 20)
	writeGoldStratum(t, store, "repo-c", "error", 1, 30)

	first, err := store.ListCases("diversified")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 {
		t.Fatalf("initial diversified = %v, want 3 pins under cap 3", caseIDs(first))
	}

	store.SetDiversifiedSize(1)
	got, err := store.ListCases("diversified")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("after lowering cap to 1, diversified = %v (n=%d), want membership to reconcile to the live cap without --refresh-diversified", caseIDs(got), len(got))
	}
}

func TestListCasesDiversified_RefreshHonorsLoweredCap(t *testing.T) {
	store := openEvalStore(t)
	store.SetDiversifiedSize(3)
	writeGoldStratum(t, store, "repo-a", "error", 1, 10)
	writeGoldStratum(t, store, "repo-b", "error", 1, 20)
	writeGoldStratum(t, store, "repo-c", "error", 1, 30)
	if _, err := store.ListCases("diversified"); err != nil {
		t.Fatal(err)
	}
	store.SetDiversifiedSize(1)
	got, err := store.RefreshDiversified()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("refresh after lowering cap to 1 = %v (n=%d), want the explicit refresh to honor the live cap", caseIDs(got), len(got))
	}
}

func TestListCasesDiversified_ZeroCapKeepsAtMostOneCasePerStratum(t *testing.T) {
	store := openEvalStore(t)
	store.SetDiversifiedSize(3)
	writeGoldStratum(t, store, "repo-heavy", "error", 5, 10)
	writeGoldStratum(t, store, "repo-mid", "warning", 3, 20)
	writeGoldStratum(t, store, "repo-light", "info", 1, 30)

	first, err := store.ListCases("diversified")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, c := range first {
		counts[c.RepoFingerprint]++
	}
	if counts["repo-heavy"] != 2 {
		t.Fatalf("setup diversified = %#v, want Hamilton to pin 2 cases in repo-heavy before lowering the cap to 0", counts)
	}

	store.SetDiversifiedSize(0)
	got, err := store.ListCases("diversified")
	if err != nil {
		t.Fatal(err)
	}
	perStratum := map[string]int{}
	for _, c := range got {
		perStratum[diversifiedStratum(c)]++
	}
	for stratum, n := range perStratum {
		if n > 1 {
			t.Fatalf("after cap 0, stratum %q has %d official cases (ids=%v); want at most one case per stratum", stratum, n, caseIDs(got))
		}
	}
}

func TestListCasesDiversified_LowCapKeepsAtMostOneCasePerStratum(t *testing.T) {
	store := openEvalStore(t)
	store.SetDiversifiedSize(3)
	writeGoldStratum(t, store, "repo-heavy", "error", 5, 10)
	writeGoldStratum(t, store, "repo-mid", "warning", 3, 20)
	writeGoldStratum(t, store, "repo-light", "info", 1, 30)
	if _, err := store.ListCases("diversified"); err != nil {
		t.Fatal(err)
	}

	store.SetDiversifiedSize(2)
	got, err := store.ListCases("diversified")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 2 {
		t.Fatalf("after cap 2, diversified = %v (n=%d), want at most the live cap", caseIDs(got), len(got))
	}
	perStratum := map[string]int{}
	for _, c := range got {
		perStratum[diversifiedStratum(c)]++
	}
	for stratum, n := range perStratum {
		if n > 1 {
			t.Fatalf("after cap 2, stratum %q has %d official cases (ids=%v); want at most one case per stratum", stratum, n, caseIDs(got))
		}
	}
}

func TestListCasesDiversified_LowerCapFillDoesNotOverAllocateOneStratum(t *testing.T) {
	store := openEvalStore(t)
	store.SetDiversifiedSize(6)
	writeGoldStratum(t, store, "repo-a", "error", 10, 10)
	writeGoldStratum(t, store, "repo-b", "error", 10, 20)
	writeGoldStratum(t, store, "repo-c", "info", 2, 30)

	first, err := store.ListCases("diversified")
	if err != nil {
		t.Fatal(err)
	}
	firstCounts := map[string]int{}
	for _, c := range first {
		firstCounts[c.RepoFingerprint]++
	}
	if firstCounts["repo-a"] < 2 || firstCounts["repo-b"] < 2 || firstCounts["repo-c"] != 0 {
		t.Fatalf("setup diversified = %#v, want duplicate pins in repo-a/repo-b and repo-c unoccupied so collapse frees seats", firstCounts)
	}

	store.SetDiversifiedSize(5)
	got, err := store.ListCases("diversified")
	if err != nil {
		t.Fatal(err)
	}
	perStratum := map[string]int{}
	for _, c := range got {
		perStratum[diversifiedStratum(c)]++
	}
	for stratum, n := range perStratum {
		if n > 1 {
			t.Fatalf("after lowering cap to 5, stratum %q has %d official cases (ids=%v counts=%v); collapse-freed seats must not reallocate multiple cases into one stratum", stratum, n, caseIDs(got), perStratum)
		}
	}
}

func TestListCasesTune_IsLabeledMinusPins(t *testing.T) {
	store := openEvalStore(t)
	store.SetDiversifiedSize(1)
	pinned := writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "pinned-gold", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
		changedFiles: []string{"main.go"},
		gold: []FindingGold{
			{ID: "a", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "a", Severity: "error", Action: "auto-fix"},
		},
		roundFindings: findingsJSON(findingSpec{ID: "a", Severity: "error", File: "main.go", Line: 1, Description: "a", Action: "auto-fix"}),
	})
	leftover := writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "tune-gold", fingerprint: "repo-b", capturedAt: 2, changedLines: 10,
		changedFiles: []string{"main.go"},
		gold: []FindingGold{
			{ID: "b", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "other.go", Line: 1, Description: "b", Severity: "error", Action: "auto-fix"},
		},
		roundFindings: findingsJSON(findingSpec{ID: "b", Severity: "error", File: "other.go", Line: 1, Description: "b", Action: "auto-fix"}),
	})
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "unlabeled", fingerprint: "repo-c", capturedAt: 3, changedLines: 10,
		changedFiles:  []string{"main.go"},
		roundFindings: findingsJSON(findingSpec{ID: "c", Severity: "error", File: "main.go", Line: 1, Description: "c", Action: "ask-user"}),
	})

	div, err := store.ListCases("diversified")
	if err != nil {
		t.Fatal(err)
	}
	if ids := caseIDs(div); len(ids) != 1 || ids[0] != pinned.ID {
		t.Fatalf("diversified = %v, want the official pin %s", ids, pinned.ID)
	}
	tune, err := store.ListCases("tune")
	if err != nil {
		t.Fatal(err)
	}
	if ids := caseIDs(tune); len(ids) != 1 || ids[0] != leftover.ID {
		t.Fatalf("tune = %v, want leftover labeled gold %s, not unlabeled or the pin", ids, leftover.ID)
	}

	if warning := mustSetSummary(t, store, "tune").Warning; warning != "" {
		t.Fatalf("tune warning = %q, unexpectedly warned that tune is empty", warning)
	}
}

func TestListCasesTune_WarnsWhenEmptyBecausePinsAreTheWholeLabeledSet(t *testing.T) {
	store := openEvalStore(t)
	store.SetDiversifiedSize(32)
	writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "only-gold", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
		changedFiles: []string{"main.go"},
		gold: []FindingGold{
			{ID: "a", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "a", Severity: "error", Action: "auto-fix"},
		},
		roundFindings: findingsJSON(findingSpec{ID: "a", Severity: "error", File: "main.go", Line: 1, Description: "a", Action: "auto-fix"}),
	})
	if _, err := store.ListCases("diversified"); err != nil {
		t.Fatal(err)
	}
	tune, err := store.ListCases("tune")
	if err != nil {
		t.Fatal(err)
	}
	if len(tune) != 0 {
		t.Fatalf("tune = %#v, want empty when every labeled case is pinned", tune)
	}
	if warning := mustSetSummary(t, store, "tune").Warning; !strings.Contains(warning, "tune is empty") {
		t.Fatalf("tune warning = %q, want a holdout warning when tune is empty", warning)
	}
}

func mustSetSummary(t *testing.T, store *Store, name string) SetSummary {
	t.Helper()
	for _, summary := range mustInspectSets(t, store) {
		if summary.Name == name {
			return summary
		}
	}
	t.Fatalf("InspectSets returned no %q summary", name)
	return SetSummary{}
}

func openEvalStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func writeGoldStratum(t *testing.T, store *Store, fingerprint, severity string, n int, capturedAtStart int64) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := fingerprint + "-" + severity + "-" + strconv.Itoa(i)
		writeSyntheticCase(t, store, syntheticCaseSpec{
			id: id, fingerprint: fingerprint, capturedAt: capturedAtStart + int64(i), changedLines: 10,
			changedFiles: []string{"main.go"},
			gold: []FindingGold{
				{ID: "g" + strconv.Itoa(i), Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "gold " + strconv.Itoa(i), Severity: severity, Action: "auto-fix"},
			},
			roundFindings: findingsJSON(findingSpec{ID: "g" + strconv.Itoa(i), Severity: severity, File: "main.go", Line: 1, Description: "gold " + strconv.Itoa(i), Action: "auto-fix"}),
		})
	}
}

func writeSyntheticCase(t *testing.T, store *Store, spec syntheticCaseSpec) Case {
	t.Helper()
	dir := store.caseDir(spec.id)
	if err := os.MkdirAll(filepath.Join(dir, "original"), 0o755); err != nil {
		t.Fatal(err)
	}
	changedFiles := spec.changedFiles
	if len(changedFiles) == 0 {
		changedFiles = []string{"main.go"}
	}
	c := Case{
		Manifest: Manifest{
			Version:         manifestVersion,
			ID:              spec.id,
			SourceRunID:     "run-" + spec.id,
			SourceRoundID:   "round-" + spec.id,
			CapturedAt:      spec.capturedAt,
			RepoFingerprint: spec.fingerprint,
			Branch:          "feature",
			DefaultBranch:   "main",
			ChangedFiles:    len(changedFiles),
			ChangedLines:    spec.changedLines,
		},
		Labels:   Labels{Version: labelsVersion, Findings: spec.gold},
		Decision: Decision{Action: "unknown"},
		Dir:      dir,
	}
	if spec.capturedAt == 0 {
		c.CapturedAt = time.Now().Unix()
	}
	roundJSON := spec.roundFindings
	if roundJSON == "" {
		roundJSON = `{"findings":[]}`
	}
	for _, item := range []struct {
		path  string
		value any
	}{
		{"manifest.json", c.Manifest},
		{"labels.json", c.Labels},
		{filepath.Join("original", "decision.json"), c.Decision},
		{filepath.Join("original", "baseline.json"), BaselineMetrics{}},
		{filepath.Join("original", "round.json"), sourceRound{ID: c.SourceRoundID, FindingsJSON: strPtr(roundJSON)}},
		{filepath.Join("original", "changed-files.json"), changedFiles},
	} {
		if err := writeJSON(filepath.Join(dir, item.path), item.value); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := loadCase(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.registerCase(loaded); err != nil {
		t.Fatal(err)
	}
	return loaded
}

type findingSpec struct {
	ID, Severity, File, Description, Action string
	Line                                    int
}

func findingsJSON(findings ...findingSpec) string {
	var b strings.Builder
	b.WriteString(`{"findings":[`)
	for i, f := range findings {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"` + f.ID + `","severity":"` + f.Severity + `","file":"` + f.File + `","line":`)
		b.WriteString(strconv.Itoa(f.Line))
		b.WriteString(`,"description":"` + f.Description + `","action":"` + f.Action + `","review_scope":"source"}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

func caseIDs(cases []Case) []string {
	ids := make([]string, len(cases))
	for i, c := range cases {
		ids[i] = c.ID
	}
	return ids
}

func strPtr(v string) *string { return &v }
