package eval

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCaptureWritesAutoFixMergedAsTruePositive(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	if err := sourceDB.SetStepRoundSelection(reviewRound.ID, strPtr(`["real-bug"]`), db.RoundSelectionSourceAutoFix); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateRunPRState(run.ID, "merged"); err != nil {
		t.Fatal(err)
	}

	gold := captureOne(t, ctx, p, sourceDB, run.ID)
	if gold.Kind != GoldTruePositive || gold.Source != goldSourceAutoFixMerged || gold.ID != "real-bug" {
		t.Fatalf("auto-fix merged gold = %#v, want recorded-auto-fix-merged true-positive", gold)
	}
}

func TestCaptureLeavesAutoFixOpenUnlabeled(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	if err := sourceDB.SetStepRoundSelection(reviewRound.ID, strPtr(`["real-bug"]`), db.RoundSelectionSourceAutoFix); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateRunPRState(run.ID, "open"); err != nil {
		t.Fatal(err)
	}
	assertCaptureUnlabeled(t, ctx, p, sourceDB, run.ID)
}

func TestCaptureLeavesAutoFixClosedUnlabeled(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	if err := sourceDB.SetStepRoundSelection(reviewRound.ID, strPtr(`["real-bug"]`), db.RoundSelectionSourceAutoFix); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateRunPRState(run.ID, "closed"); err != nil {
		t.Fatal(err)
	}
	assertCaptureUnlabeled(t, ctx, p, sourceDB, run.ID)
}

// A fix that was reverted (the same issue is raised again by a later round)
// does not unmake the human's recorded decision to fix it on a run that merged:
// the finding is still gold-standard evidence of a real issue.
func TestCaptureLabelsSelectedAutoFixAsTruePositiveEvenWhenLaterRoundReRaisesIt(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, firstRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	if err := sourceDB.SetStepRoundSelection(firstRound.ID, strPtr(`["real-bug"]`), db.RoundSelectionSourceAutoFix); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateRunPRState(run.ID, "merged"); err != nil {
		t.Fatal(err)
	}
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	stillRaised := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"still present","risk_scope":"source-or-external"}`
	if _, err := sourceDB.InsertReviewStepRoundWithProvenance(steps[0].ID, 2, "auto_fix", &stillRaised, nil, run.HeadSHA, stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), firstRound.GlobalConfigYAML, firstRound.RepoConfigYAML, 25); err != nil {
		t.Fatal(err)
	}

	cases := captureAll(t, ctx, p, sourceDB, run.ID)
	if len(cases) != 2 {
		t.Fatalf("captured cases = %d, want both review rounds", len(cases))
	}
	for _, c := range cases {
		if c.SourceRoundID != firstRound.ID {
			continue
		}
		if len(c.Labels.Findings) != 1 {
			t.Fatalf("re-raised auto-fix round labels = %#v, want one gold finding", c.Labels)
		}
		gold := c.Labels.Findings[0]
		if gold.Kind != GoldTruePositive || gold.Source != goldSourceAutoFixMerged || gold.ID != "real-bug" {
			t.Fatalf("re-raised auto-fix gold = %#v, want recorded-auto-fix-merged true-positive from the recorded fix decision", gold)
		}
	}
}

// Each round carries its own recorded decision, so a later round that rewrites
// the finding under a new id does not retroactively unlabel the earlier one.
func TestCaptureLabelsBothRoundsOfASupersededAutoFix(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, firstRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	if err := sourceDB.SetStepRoundSelection(firstRound.ID, strPtr(`["real-bug"]`), db.RoundSelectionSourceAutoFix); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateRunPRState(run.ID, "merged"); err != nil {
		t.Fatal(err)
	}
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := `{"findings":[{"id":"real-bug-v2","severity":"error","file":"main.go","line":3,"description":"bug","action":"auto-fix","review_scope":"source"}],"risk_level":"high","risk_rationale":"rewritten","risk_scope":"source-or-external"}`
	second, err := sourceDB.InsertReviewStepRoundWithProvenance(steps[0].ID, 2, "auto_fix", &rewritten, nil, run.HeadSHA, stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), firstRound.GlobalConfigYAML, firstRound.RepoConfigYAML, 25)
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.SetStepRoundSelection(second.ID, strPtr(`["real-bug-v2"]`), db.RoundSelectionSourceAutoFix); err != nil {
		t.Fatal(err)
	}

	cases := captureAll(t, ctx, p, sourceDB, run.ID)
	byRound := map[string]Labels{}
	for _, c := range cases {
		byRound[c.SourceRoundID] = c.Labels
	}
	first := byRound[firstRound.ID]
	if len(first.Findings) != 1 || first.Findings[0].Kind != GoldTruePositive || first.Findings[0].Source != goldSourceAutoFixMerged || first.Findings[0].ID != "real-bug" {
		t.Fatalf("superseded first-round gold = %#v, want auto-fix-merged TP from its own recorded fix decision", first)
	}
	got := byRound[second.ID]
	if len(got.Findings) != 1 || got.Findings[0].Kind != GoldTruePositive || got.Findings[0].Source != goldSourceAutoFixMerged || got.Findings[0].ID != "real-bug-v2" {
		t.Fatalf("landed later-round gold = %#v, want auto-fix-merged TP for the rewritten id", got)
	}
}

func TestCaptureWritesShippedUnfixedAsFalsePositive(t *testing.T) {
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

	gold := captureOne(t, ctx, p, sourceDB, run.ID)
	if gold.Kind != GoldFalsePositive || gold.Source != goldSourceShippedUnfixed || gold.ID != "real-bug" {
		t.Fatalf("shipped-unfixed gold = %#v, want recorded-shipped-unfixed false-positive", gold)
	}
}

// The human resolved this round without selecting the finding and the run
// merged, so it is shipped-unfixed false-positive gold. A later round raising a
// different issue does not change what was decided about this one.
func TestCaptureLabelsUnselectedFindingAsFalsePositiveEvenWhenALaterRoundDropsIt(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, firstRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	if err := sourceDB.SetStepRoundSelection(firstRound.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateStepStatus(steps[0].ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	later := `{"findings":[{"id":"other-bug","severity":"warning","file":"main.go","line":1,"description":"style","action":"ask-user","review_scope":"source"}],"risk_level":"low","risk_rationale":"different issue","risk_scope":"source-or-external"}`
	if _, err := sourceDB.InsertReviewStepRoundWithProvenance(steps[0].ID, 2, "auto_fix", &later, nil, run.HeadSHA, stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), firstRound.GlobalConfigYAML, firstRound.RepoConfigYAML, 25); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateRunPRState(run.ID, "merged"); err != nil {
		t.Fatal(err)
	}

	cases := captureAll(t, ctx, p, sourceDB, run.ID)
	byRound := map[string]Labels{}
	for _, c := range cases {
		byRound[c.SourceRoundID] = c.Labels
	}
	first := byRound[firstRound.ID]
	if len(first.Findings) != 1 || first.Findings[0].Kind != GoldFalsePositive || first.Findings[0].Source != goldSourceShippedUnfixed || first.Findings[0].ID != "real-bug" {
		t.Fatalf("first-round gold = %#v, want shipped-unfixed FP from its own skip decision", first)
	}
	var second Labels
	for id, labels := range byRound {
		if id != firstRound.ID {
			second = labels
			break
		}
	}
	if len(second.Findings) != 1 || second.Findings[0].Kind != GoldFalsePositive || second.Findings[0].Source != goldSourceShippedUnfixed || second.Findings[0].ID != "other-bug" {
		t.Fatalf("last-round gold = %#v, want shipped-unfixed FP for the still-raised finding", second)
	}
}

func TestCaptureWritesEmptyActionShippedUnfixedAsFalsePositive(t *testing.T) {
	ctx := context.Background()
	findings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	p, sourceDB, run, _, reviewRound := setupCapturedRunWithFindings(t, ctx, findings)
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

	gold := captureOne(t, ctx, p, sourceDB, run.ID)
	if gold.Kind != GoldFalsePositive || gold.Source != goldSourceShippedUnfixed || gold.ID != "real-bug" {
		t.Fatalf("empty-action shipped-unfixed gold = %#v, want recorded-shipped-unfixed false-positive", gold)
	}
}

func TestCaptureDoesNotLabelNoOpShippedUnfixed(t *testing.T) {
	ctx := context.Background()
	findings := `{"findings":[{"id":"note","severity":"info","file":"main.go","line":1,"description":"style","action":"no-op","review_scope":"source"}],"risk_level":"low","risk_rationale":"note","risk_scope":"source-or-external"}`
	p, sourceDB, run, _, reviewRound := setupCapturedRunWithFindings(t, ctx, findings)
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
	assertCaptureUnlabeled(t, ctx, p, sourceDB, run.ID)
}

func TestCaptureShippedUnfixedLeavesSelectedSiblingAsTP(t *testing.T) {
	ctx := context.Background()
	findings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"auto-fix","review_scope":"source"},{"id":"noise","severity":"warning","file":"main.go","line":1,"description":"style","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	p, sourceDB, run, _, reviewRound := setupCapturedRunWithFindings(t, ctx, findings)
	defer sourceDB.Close()
	if err := sourceDB.SetStepRoundSelection(reviewRound.ID, strPtr(`["real-bug"]`), db.RoundSelectionSourceAutoFix); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateRunPRState(run.ID, "merged"); err != nil {
		t.Fatal(err)
	}

	cases := captureAll(t, ctx, p, sourceDB, run.ID)
	if len(cases) != 1 {
		t.Fatalf("captured cases = %d, want 1", len(cases))
	}
	byID := map[string]FindingGold{}
	for _, gold := range cases[0].Labels.Findings {
		byID[gold.ID] = gold
	}
	if got := byID["real-bug"]; got.Kind != GoldTruePositive || got.Source != goldSourceAutoFixMerged {
		t.Fatalf("selected auto-fix gold = %#v, want TP", got)
	}
	if got := byID["noise"]; got.Kind != GoldFalsePositive || got.Source != goldSourceShippedUnfixed {
		t.Fatalf("unselected sibling gold = %#v, want shipped-unfixed FP", got)
	}
}

func TestRelabelPromotesAutoFixWhenPRLaterMerges(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	if err := sourceDB.SetStepRoundSelection(reviewRound.ID, strPtr(`["real-bug"]`), db.RoundSelectionSourceAutoFix); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateRunPRState(run.ID, "open"); err != nil {
		t.Fatal(err)
	}

	store := mustOpenEval(t, p)
	first, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Labels.HasGold() {
		t.Fatalf("open auto-fix capture = %#v, want unlabeled until merge", first)
	}

	if err := sourceDB.UpdateRunPRState(run.ID, "merged"); err != nil {
		t.Fatal(err)
	}
	relabeled, err := RelabelRun(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(relabeled) != 1 || len(relabeled[0].Labels.Findings) != 1 {
		t.Fatalf("relabeled = %#v, want auto-fix-merged true-positive", relabeled)
	}
	gold := relabeled[0].Labels.Findings[0]
	if gold.Kind != GoldTruePositive || gold.Source != goldSourceAutoFixMerged {
		t.Fatalf("late-merge gold = %#v, want recorded-auto-fix-merged", gold)
	}

	again, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || len(again[0].Labels.Findings) != 1 || again[0].Labels.Findings[0].Source != goldSourceAutoFixMerged {
		t.Fatalf("recapture after merge = %#v, want additive relabel rather than a no-op skip", again)
	}
}

func TestRelabelDoesNotClobberAdjudicatedLabels(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	if err := sourceDB.SetStepRoundSelection(reviewRound.ID, strPtr(`["real-bug"]`), db.RoundSelectionSourceAutoFix); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateRunPRState(run.ID, "open"); err != nil {
		t.Fatal(err)
	}

	store := mustOpenEval(t, p)
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	labels := cases[0].Labels
	labels.Findings = []FindingGold{{
		ID:          "real-bug",
		Kind:        GoldFalsePositive,
		Source:      "adjudicated-user",
		File:        "main.go",
		Line:        3,
		Description: "bug",
		Severity:    "error",
	}}
	if err := writeJSON(filepath.Join(cases[0].Dir, "labels.json"), labels); err != nil {
		t.Fatal(err)
	}

	if err := sourceDB.UpdateRunPRState(run.ID, "merged"); err != nil {
		t.Fatal(err)
	}
	relabeled, err := RelabelRun(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(relabeled) != 1 || len(relabeled[0].Labels.Findings) != 1 {
		t.Fatalf("relabeled = %#v, want the adjudicated label preserved", relabeled)
	}
	got := relabeled[0].Labels.Findings[0]
	if got.Kind != GoldFalsePositive || got.Source != "adjudicated-user" {
		t.Fatalf("adjudicated gold = %#v, want auto-relabel to leave it alone", got)
	}
}

// Round presence used to veto this label: the finding survived into round 2 but
// was gone by the final round, so the earlier rounds stayed unlabeled. Labeling
// now keys off each round's own recorded decision, which said "ship it".
func TestCaptureWritesShippedUnfixedEvenWhenTheFinalRoundNoLongerRaisesIt(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, firstRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	if err := sourceDB.SetStepRoundSelection(firstRound.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateStepStatus(steps[0].ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	stillRaised := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"still present","risk_scope":"source-or-external"}`
	if _, err := sourceDB.InsertReviewStepRoundWithProvenance(steps[0].ID, 2, "auto_fix", &stillRaised, nil, run.HeadSHA, stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), firstRound.GlobalConfigYAML, firstRound.RepoConfigYAML, 25); err != nil {
		t.Fatal(err)
	}
	final := `{"findings":[{"id":"other-bug","severity":"warning","file":"main.go","line":1,"description":"style","action":"ask-user","review_scope":"source"}],"risk_level":"low","risk_rationale":"different issue","risk_scope":"source-or-external"}`
	if _, err := sourceDB.InsertReviewStepRoundWithProvenance(steps[0].ID, 3, "auto_fix", &final, nil, run.HeadSHA, stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), firstRound.GlobalConfigYAML, firstRound.RepoConfigYAML, 25); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateRunPRState(run.ID, "merged"); err != nil {
		t.Fatal(err)
	}

	cases := captureAll(t, ctx, p, sourceDB, run.ID)
	byRound := map[string]Labels{}
	for _, c := range cases {
		byRound[c.SourceRoundID] = c.Labels
	}
	first := byRound[firstRound.ID]
	if len(first.Findings) != 1 || first.Findings[0].Kind != GoldFalsePositive || first.Findings[0].Source != goldSourceShippedUnfixed || first.Findings[0].ID != "real-bug" {
		t.Fatalf("first-round gold = %#v, want shipped-unfixed FP keyed on this round's skip decision, not on the final round's finding list", first)
	}
}

// Derived merge gold is recomputed, not appended: once the round records that
// the human selected the finding for Fix, the earlier shipped-unfixed FP is gone
// rather than kept alongside the true-positive label.
func TestRelabelReplacesShippedUnfixedWhenTheRoundLaterRecordsAFixDecision(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, firstRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	if err := sourceDB.SetStepRoundSelection(firstRound.ID, nil, ""); err != nil {
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
	first, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(first[0].Labels.Findings) != 1 || first[0].Labels.Findings[0].Source != goldSourceShippedUnfixed {
		t.Fatalf("initial capture = %#v, want shipped-unfixed FP before the later round exists", first)
	}

	if err := sourceDB.SetStepRoundSelection(firstRound.ID, strPtr(`["real-bug"]`), db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	relabeled, err := RelabelRun(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(relabeled) != 1 || len(relabeled[0].Labels.Findings) != 1 {
		t.Fatalf("relabeled = %#v, want exactly the recomputed user-fix gold", relabeled)
	}
	gold := relabeled[0].Labels.Findings[0]
	if gold.Kind != GoldTruePositive || gold.Source != goldSourceUserFix {
		t.Fatalf("relabeled gold = %#v, want the obsolete shipped-unfixed FP replaced by the recorded user Fix", gold)
	}
}

func TestMergeGoldClearsStoredShippedUnfixedWhenRecomputedUnlabeled(t *testing.T) {
	existing := Labels{
		Version: labelsVersion,
		Findings: []FindingGold{{
			ID:          "real-bug",
			Kind:        GoldFalsePositive,
			Source:      goldSourceShippedUnfixed,
			File:        "main.go",
			Line:        3,
			Description: "bug",
			Severity:    "error",
			Action:      "ask-user",
		}},
		QueuedCandidateFindings: 2,
	}
	got := mergeGold(existing, Labels{Version: labelsVersion})
	if got.HasGold() {
		t.Fatalf("mergeGold = %#v, want the stored shipped-unfixed FP cleared when recomputed gold has none", got)
	}
	if got.QueuedCandidateFindings != 2 {
		t.Fatalf("queued candidate findings = %d, want the existing queue preserved while derived gold is dropped", got.QueuedCandidateFindings)
	}
}

func TestRelabelClearsStoredShippedUnfixedFPWhenRecomputedUnlabeled(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, firstRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	if err := sourceDB.SetStepRoundSelection(firstRound.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateStepStatus(steps[0].ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	// The PR never merged, so approve-with-findings supports no label at all.
	if err := sourceDB.UpdateRunPRState(run.ID, "open"); err != nil {
		t.Fatal(err)
	}

	store := mustOpenEval(t, p)
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var first Case
	for _, c := range cases {
		if c.SourceRoundID == firstRound.ID {
			first = c
			break
		}
	}
	if first.Dir == "" || first.Labels.HasGold() {
		t.Fatalf("first-round capture = %#v, want unlabeled so the stored FP is planted, not freshly written", first)
	}
	stale := first.Labels
	stale.Findings = []FindingGold{{
		ID:          "real-bug",
		Kind:        GoldFalsePositive,
		Source:      goldSourceShippedUnfixed,
		File:        "main.go",
		Line:        3,
		Description: "bug",
		Severity:    "error",
		Action:      "ask-user",
	}}
	if err := writeJSON(filepath.Join(first.Dir, "labels.json"), stale); err != nil {
		t.Fatal(err)
	}

	relabeled, err := RelabelRun(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got Case
	for _, c := range relabeled {
		if c.SourceRoundID == firstRound.ID {
			got = c
			break
		}
	}
	if got.Dir == "" {
		t.Fatal("relabel did not return the first-round case")
	}
	for _, gold := range got.Labels.Findings {
		if gold.ID == "real-bug" && gold.Source == goldSourceShippedUnfixed {
			t.Fatalf("relabeled in-memory labels = %#v, want stored shipped-unfixed FP cleared", got.Labels)
		}
	}
	onDisk, err := loadCase(got.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, gold := range onDisk.Labels.Findings {
		if gold.ID == "real-bug" && gold.Source == goldSourceShippedUnfixed {
			t.Fatalf("labels.json = %#v, want stored shipped-unfixed FP removed from disk, not append-only retention", onDisk.Labels)
		}
	}
}

// TestGoldFromRoundLabelsByRecordedDecision pins the labeling contract itself:
// what a finding is labeled follows from the round's recorded fix-vs-skip
// decision plus the source run's merge state, and from nothing else.
func TestGoldFromRoundLabelsByRecordedDecision(t *testing.T) {
	raised := findingsJSON(findingSpec{ID: "real-bug", Severity: "error", File: "main.go", Line: 3, Description: "bug", Action: "auto-fix"})
	informational := findingsJSON(findingSpec{ID: "note", Severity: "info", File: "main.go", Line: 3, Description: "style note", Action: "no-op"})

	for _, tc := range []struct {
		name     string
		findings string
		decision Decision
		prState  string
		wantKind string
		wantGold string
	}{
		{
			name: "selected for fix and merged is true-positive gold", findings: raised,
			decision: Decision{Action: decisionFix, SelectionSource: db.RoundSelectionSourceAutoFix, SelectedFindingIDs: []string{"real-bug"}},
			prState:  "merged", wantKind: GoldTruePositive, wantGold: goldSourceAutoFixMerged,
		},
		{
			name: "user selected for fix needs no merge", findings: raised,
			decision: Decision{Action: decisionFix, SelectionSource: db.RoundSelectionSourceUser, SelectedFindingIDs: []string{"real-bug"}},
			prState:  "open", wantKind: GoldTruePositive, wantGold: goldSourceUserFix,
		},
		{
			name: "skipped and merged is shipped-unfixed false-positive gold", findings: raised,
			decision: Decision{Action: decisionApprove},
			prState:  "merged", wantKind: GoldFalsePositive, wantGold: goldSourceShippedUnfixed,
		},
		{
			name: "auto-fix selection leaves an unselected sibling shipped unfixed", findings: raised,
			decision: Decision{Action: decisionFix, SelectionSource: db.RoundSelectionSourceAutoFix, SelectedFindingIDs: []string{"other"}},
			prState:  "merged", wantKind: GoldFalsePositive, wantGold: goldSourceShippedUnfixed,
		},
		{
			name: "no recorded decision stays unlabeled even on a merged run", findings: raised,
			decision: Decision{Action: decisionUnknown},
			prState:  "merged",
		},
		{
			name: "an aborted round records no decision", findings: raised,
			decision: Decision{Action: decisionAbort},
			prState:  "merged",
		},
		{
			name: "skipped without a merge stays unlabeled", findings: raised,
			decision: Decision{Action: decisionApprove},
			prState:  "open",
		},
		{
			name: "informational no-op findings are never labeled", findings: informational,
			decision: Decision{Action: decisionApprove},
			prState:  "merged",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			round := &db.StepRound{Round: 1, FindingsJSON: strPtr(tc.findings)}
			got := goldFromRound(round, tc.decision, tc.prState)
			if tc.wantKind == "" {
				if got.HasGold() {
					t.Fatalf("labels = %#v, want unlabeled", got)
				}
				return
			}
			if len(got.Findings) != 1 {
				t.Fatalf("labels = %#v, want exactly one gold finding", got)
			}
			gold := got.Findings[0]
			if gold.Kind != tc.wantKind || gold.Source != tc.wantGold {
				t.Fatalf("gold = %#v, want %s gold from %s", gold, tc.wantKind, tc.wantGold)
			}
		})
	}
}

func TestCaptureKeepsUserFixWithoutMerge(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	gold := captureOne(t, ctx, p, sourceDB, run.ID)
	if gold.Source != goldSourceUserFix || gold.Kind != GoldTruePositive {
		t.Fatalf("user-fix gold = %#v, want recorded-user-fix without merge", gold)
	}
}

func captureAll(t *testing.T, ctx context.Context, p *paths.Paths, sourceDB *db.DB, runID string) []Case {
	t.Helper()
	store := mustOpenEval(t, p)
	cases, err := Capture(ctx, store, p, sourceDB, runID)
	if err != nil {
		t.Fatal(err)
	}
	return cases
}

func captureOne(t *testing.T, ctx context.Context, p *paths.Paths, sourceDB *db.DB, runID string) FindingGold {
	t.Helper()
	cases := captureAll(t, ctx, p, sourceDB, runID)
	if len(cases) != 1 || len(cases[0].Labels.Findings) != 1 {
		t.Fatalf("captured gold = %#v, want exactly one labeled finding", cases)
	}
	return cases[0].Labels.Findings[0]
}

func assertCaptureUnlabeled(t *testing.T, ctx context.Context, p *paths.Paths, sourceDB *db.DB, runID string) {
	t.Helper()
	cases := captureAll(t, ctx, p, sourceDB, runID)
	if len(cases) != 1 || cases[0].Labels.HasGold() {
		t.Fatalf("captured labels = %#v, want unlabeled", cases)
	}
}

func mustOpenEval(t *testing.T, p *paths.Paths) *Store {
	t.Helper()
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
