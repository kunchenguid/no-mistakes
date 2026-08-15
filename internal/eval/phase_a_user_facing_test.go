package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestPhaseAUserFacingTranscripts exercises the same public renderers the
// `eval sets` / `eval report` / `eval capture` commands print, and writes those
// transcripts as reviewer-visible evidence when NM_EVIDENCE_DIR is set.
func TestPhaseAUserFacingTranscripts(t *testing.T) {
	evidenceDir := strings.TrimSpace(os.Getenv("NM_EVIDENCE_DIR"))
	write := func(name, body string) {
		t.Helper()
		if evidenceDir == "" {
			return
		}
		if err := os.WriteFile(filepath.Join(evidenceDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write evidence %s: %v", name, err)
		}
	}

	t.Run("empty gold warns and does not fill diversified", func(t *testing.T) {
		store := openEvalStore(t)
		writeSyntheticCase(t, store, syntheticCaseSpec{
			id: "unlabeled-only", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
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
		output := RenderSets(mustInspectSets(t, store))
		if !strings.Contains(output, "diversified: 0 cases") || !strings.Contains(output, "no labeled gold") {
			t.Fatalf("sets output = %q, want empty diversified plus a gold-only warning", output)
		}
		write("eval-sets-empty-gold.txt", output)
	})

	t.Run("official holdout vs tune leftover", func(t *testing.T) {
		store := openEvalStore(t)
		store.SetDiversifiedSize(1)
		pinned := writeSyntheticCase(t, store, syntheticCaseSpec{
			id: "official-pin", fingerprint: "repo-a", capturedAt: 1, changedLines: 10,
			changedFiles: []string{"main.go"},
			gold: []FindingGold{
				{ID: "a", Kind: GoldTruePositive, Source: goldSourceUserFix, File: "main.go", Line: 1, Description: "a", Severity: "error", Action: "auto-fix"},
			},
			roundFindings: findingsJSON(findingSpec{ID: "a", Severity: "error", File: "main.go", Line: 1, Description: "a", Action: "auto-fix"}),
		})
		tune := writeSyntheticCase(t, store, syntheticCaseSpec{
			id: "tune-leftover", fingerprint: "repo-b", capturedAt: 2, changedLines: 10,
			changedFiles: []string{"other.go"},
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
			t.Fatalf("diversified = %v, want official pin %s", ids, pinned.ID)
		}
		leftover, err := store.ListCases("tune")
		if err != nil {
			t.Fatal(err)
		}
		if ids := caseIDs(leftover); len(ids) != 1 || ids[0] != tune.ID {
			t.Fatalf("tune = %v, want leftover labeled gold %s", ids, tune.ID)
		}
		output := RenderSets(mustInspectSets(t, store))
		if strings.Contains(output, "tune is empty") {
			t.Fatalf("sets output = %q, unexpectedly warned that tune is empty", output)
		}
		write("eval-sets-official-vs-tune.txt", output)
	})

	t.Run("live cap shrink keeps at most one official case per stratum", func(t *testing.T) {
		store := openEvalStore(t)
		store.SetDiversifiedSize(3)
		writeGoldStratum(t, store, "repo-heavy", "error", 5, 10)
		writeGoldStratum(t, store, "repo-mid", "warning", 3, 20)
		writeGoldStratum(t, store, "repo-light", "info", 1, 30)
		before, err := store.ListCases("diversified")
		if err != nil {
			t.Fatal(err)
		}
		beforeCounts := map[string]int{}
		for _, c := range before {
			beforeCounts[c.RepoFingerprint]++
		}
		if beforeCounts["repo-heavy"] != 2 {
			t.Fatalf("setup diversified = %#v, want Hamilton to pin 2 cases in repo-heavy", beforeCounts)
		}
		beforeOut := RenderSets(mustInspectSets(t, store))

		store.SetDiversifiedSize(0)
		after, err := store.ListCases("diversified")
		if err != nil {
			t.Fatal(err)
		}
		perStratum := map[string]int{}
		for _, c := range after {
			perStratum[diversifiedStratum(c)]++
		}
		for stratum, n := range perStratum {
			if n > 1 {
				t.Fatalf("after cap 0, stratum %q has %d official cases (ids=%v)", stratum, n, caseIDs(after))
			}
		}
		afterOut := RenderSets(mustInspectSets(t, store))
		write("eval-sets-cap-reconcile.txt", "BEFORE (cap 3, Hamilton extras in one stratum):\n"+beforeOut+"\nAFTER (cap 0, at most one official case per stratum):\n"+afterOut)
	})

	t.Run("lower cap fill does not dump leftover seats into one unoccupied stratum", func(t *testing.T) {
		store := openEvalStore(t)
		store.SetDiversifiedSize(6)
		writeGoldStratum(t, store, "repo-a", "error", 10, 10)
		writeGoldStratum(t, store, "repo-b", "error", 10, 20)
		writeGoldStratum(t, store, "repo-c", "info", 2, 30)
		before, err := store.ListCases("diversified")
		if err != nil {
			t.Fatal(err)
		}
		beforeCounts := map[string]int{}
		for _, c := range before {
			beforeCounts[c.RepoFingerprint]++
		}
		if beforeCounts["repo-a"] < 2 || beforeCounts["repo-b"] < 2 || beforeCounts["repo-c"] != 0 {
			t.Fatalf("setup diversified = %#v, want duplicate pins in repo-a/repo-b and repo-c unoccupied", beforeCounts)
		}
		beforeOut := RenderSets(mustInspectSets(t, store))

		store.SetDiversifiedSize(5)
		after, err := store.ListCases("diversified")
		if err != nil {
			t.Fatal(err)
		}
		perStratum := map[string]int{}
		for _, c := range after {
			perStratum[diversifiedStratum(c)]++
		}
		for stratum, n := range perStratum {
			if n > 1 {
				t.Fatalf("after cap 5, stratum %q has %d official cases (ids=%v)", stratum, n, caseIDs(after))
			}
		}
		afterOut := RenderSets(mustInspectSets(t, store))
		write("eval-sets-lower-cap-no-overallocate.txt", "BEFORE (cap 6, Hamilton pins duplicates in two strata and leaves a third unoccupied):\n"+beforeOut+"\nAFTER (cap 5, collapse-freed seats must not reallocate multiple official cases into one stratum):\n"+afterOut)
	})

	t.Run("report withholds F1 without FP gold and headlines it when FP gold exists", func(t *testing.T) {
		noFP := SummarizeEvaluations([]Evaluation{{
			Candidate: "codex+test", Status: "completed", HasFindingGold: true, GoldCount: 2,
			TruePositive: 2, Pending: 1,
		}})
		noFPOut := RenderReport([]CandidateReport{{Cohort: "official-holdout", Summary: noFP, RepeatCount: 1}})
		if !strings.Contains(noFPOut, "recall: 100.0%") || !strings.Contains(noFPOut, "F1: withheld") {
			t.Fatalf("no-FP report = %q, want recall plus withheld F1", noFPOut)
		}
		if strings.Contains(noFPOut, "F1:") && !strings.Contains(noFPOut, "F1: withheld") {
			t.Fatalf("no-FP report = %q, headline F1 must not appear without FP gold", noFPOut)
		}
		write("eval-report-f1-withheld.txt", noFPOut)

		withFP := SummarizeEvaluations([]Evaluation{{
			Candidate: "codex+test", Status: "completed", HasFindingGold: true, GoldCount: 2,
			TruePositive: 2, FalsePositive: 1, FalsePositiveGold: 1,
		}})
		withFPOut := RenderReport([]CandidateReport{{Cohort: "official-holdout", Summary: withFP, RepeatCount: 1}})
		if !strings.Contains(withFPOut, "F1:") || strings.Contains(withFPOut, "F1: withheld") {
			t.Fatalf("FP-gold report = %q, want headline F1", withFPOut)
		}
		write("eval-report-f1-headline.txt", withFPOut)
	})

	t.Run("capture labels shipped-unfixed as FP and leaves dropped reraise unlabeled", func(t *testing.T) {
		ctx := context.Background()
		p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
		defer sourceDB.Close()
		if err := sourceDB.SetStepRoundSelection(reviewRound.ID, nil, ""); err != nil {
			t.Fatal(err)
		}
		steps, err := sourceDB.GetStepsByRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := sourceDB.UpdateStepStatus(steps[0].ID, types.StepStatusCompleted); err != nil {
			t.Fatal(err)
		}
		if err := sourceDB.UpdateRunPRState(run.ID, "merged"); err != nil {
			t.Fatal(err)
		}
		store := mustOpenEval(t, p)
		cases, err := Capture(ctx, store, p, sourceDB, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(cases) != 1 || len(cases[0].Labels.Findings) != 1 {
			t.Fatalf("shipped-unfixed capture = %#v, want one FP gold finding", cases)
		}
		gold := cases[0].Labels.Findings[0]
		if gold.Kind != GoldFalsePositive || gold.Source != goldSourceShippedUnfixed {
			t.Fatalf("shipped-unfixed gold = %#v, want recorded-shipped-unfixed false-positive", gold)
		}
		labelsJSON, err := json.MarshalIndent(cases[0].Labels, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		write("labels-shipped-unfixed.json", string(labelsJSON)+"\n")

		p2, sourceDB2, run2, _, firstRound := setupCapturedRun(t, ctx)
		defer sourceDB2.Close()
		if err := sourceDB2.SetStepRoundSelection(firstRound.ID, nil, ""); err != nil {
			t.Fatal(err)
		}
		steps2, err := sourceDB2.GetStepsByRun(run2.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := sourceDB2.UpdateStepStatus(steps2[0].ID, types.StepStatusCompleted); err != nil {
			t.Fatal(err)
		}
		stillRaised := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"still present","risk_scope":"source-or-external"}`
		if _, err := sourceDB2.InsertReviewStepRoundWithProvenance(steps2[0].ID, 2, "auto_fix", &stillRaised, nil, run2.HeadSHA, stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), firstRound.GlobalConfigYAML, firstRound.RepoConfigYAML, 25); err != nil {
			t.Fatal(err)
		}
		final := `{"findings":[{"id":"other-bug","severity":"warning","file":"main.go","line":1,"description":"style","action":"ask-user","review_scope":"source"}],"risk_level":"low","risk_rationale":"different issue","risk_scope":"source-or-external"}`
		if _, err := sourceDB2.InsertReviewStepRoundWithProvenance(steps2[0].ID, 3, "auto_fix", &final, nil, run2.HeadSHA, stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), firstRound.GlobalConfigYAML, firstRound.RepoConfigYAML, 25); err != nil {
			t.Fatal(err)
		}
		if err := sourceDB2.UpdateRunPRState(run2.ID, "merged"); err != nil {
			t.Fatal(err)
		}
		dropped := captureAll(t, ctx, p2, sourceDB2, run2.ID)
		byRound := map[string]Labels{}
		for _, c := range dropped {
			byRound[c.SourceRoundID] = c.Labels
		}
		if byRound[firstRound.ID].HasGold() {
			t.Fatalf("dropped-reraise first-round labels = %#v, want unlabeled", byRound[firstRound.ID])
		}
		type droppedRow struct {
			RoundID string `json:"source_round_id"`
			Labels  Labels `json:"labels"`
		}
		rows := make([]droppedRow, 0, len(dropped))
		for _, c := range dropped {
			rows = append(rows, droppedRow{RoundID: c.SourceRoundID, Labels: c.Labels})
		}
		droppedJSON, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		write("labels-dropped-after-reraise.json", string(droppedJSON)+"\n")
	})
}
