package eval

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ciObservationFindings is a settled CI observation carrying one finding of
// every category the classifier emits: a failing check attributed to the job
// (ci-check), a review bot's comment with a code location (ci-review-bot), a
// provider/infra outcome (ci-transient), and a second review-bot finding that
// the human left unselected at the gate. The selection below fixes the first
// three; the fourth is dismissed.
const ciObservationFindings = `{"findings":[
	{"id":"ci-1","severity":"error","action":"auto-fix","category":"ci-check","check":"build","check_id":"gh:build:1","description":"CI check failing: build - provider reported failure"},
	{"id":"ci-2","severity":"warning","action":"ask-user","category":"ci-review-bot","check":"greptile","check_id":"gh:greptile:1","file":"pkg/svc.go","line":42,"description":"greptile[bot]: possible nil dereference here"},
	{"id":"ci-3","severity":"warning","action":"ask-user","category":"ci-transient","check":"flaky-runner","check_id":"gh:flaky:1","description":"runner was cancelled by the provider"},
	{"id":"ci-4","severity":"warning","action":"ask-user","category":"ci-review-bot","check":"greptile","check_id":"gh:greptile:1","file":"pkg/other.go","line":7,"description":"greptile[bot]: style nit the human dismissed"}
],"summary":"1 CI check failing; review bot needs a decision (2 findings); 1 transient"}`

// setupRunWithGreenReviewAndCI builds on the captured-run fixture: it adds a
// green (clean) review round so a false-negative has a case to attach to, then
// a CI step with one settled observation round. selected is the JSON array of
// finding IDs the pipeline chose to fix after that observation; overrideReason,
// when non-empty, marks the CI step as passed-with-override. Both the review and
// CI steps are completed and the run is marked completed.
func setupRunWithGreenReviewAndCI(t *testing.T, ctx context.Context, selected, overrideReason string) (*paths.Paths, *db.DB, *db.Run, *db.StepRound) {
	t.Helper()
	return setupRunWithCIRepairEvidence(t, ctx, selected, overrideReason, true, "fixed and published", true, false)
}

func setupRunWithCIRepairEvidence(t *testing.T, ctx context.Context, selected, overrideReason string, repairPublished bool, repairSummary string, checksPassed, declaredNoCI bool) (*paths.Paths, *db.DB, *db.Run, *db.StepRound) {
	t.Helper()
	p, sourceDB, run, _, firstRound := setupCapturedRun(t, ctx)

	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	reviewStepID := steps[0].ID
	clean := `{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`
	greenRound, err := sourceDB.InsertReviewStepRoundWithProvenance(reviewStepID, 2, "initial", &clean, nil, run.HeadSHA, stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), firstRound.GlobalConfigYAML, firstRound.RepoConfigYAML, 20)
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateStepStatus(reviewStepID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}

	ciStep, err := sourceDB.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	observation := ciObservationFindings
	ciRound, err := sourceDB.InsertStepRound(ciStep.ID, 1, "initial", &observation, nil, 30)
	if err != nil {
		t.Fatal(err)
	}
	if selected != "" {
		if err := sourceDB.SetStepRoundSelection(ciRound.ID, &selected, db.RoundSelectionSourceAutoFix); err != nil {
			t.Fatal(err)
		}
		var summary *string
		if repairSummary != "" {
			summary = &repairSummary
		}
		if _, err := sourceDB.InsertStepRoundWithRepair(ciStep.ID, 2, "auto_fix", nil, summary, repairPublished, 40); err != nil {
			t.Fatal(err)
		}
	}
	if err := sourceDB.SetRunCIReadyWithReason(run.ID, checksPassed, declaredNoCI); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateStepStatus(ciStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if overrideReason != "" {
		if err := sourceDB.SetStepOverrideReason(ciStep.ID, overrideReason); err != nil {
			t.Fatal(err)
		}
	}
	if err := sourceDB.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	return p, sourceDB, run, greenRound
}

func TestCIFalseNegativesFromRun_IngestsFixedCheckAndReviewBotOnly(t *testing.T) {
	ctx := context.Background()
	// ci-1 (ci-check) and ci-2 (ci-review-bot) are fixed; ci-3 (ci-transient) is
	// selected too but must still be excluded by category; ci-4 (ci-review-bot)
	// is not selected, so it was dismissed and must be excluded.
	_, sourceDB, run, _ := setupRunWithGreenReviewAndCI(t, ctx, `["ci-1","ci-2","ci-3"]`, "")
	defer sourceDB.Close()

	gold, err := CIFalseNegativesFromRun(sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	byDescription := map[string]FindingGold{}
	for _, g := range gold {
		byDescription[g.Description] = g
		if g.Kind != GoldFalseNegative || g.Source != goldSourceCIFalseNegative {
			t.Fatalf("gold %#v: want false-negative kind and CI source", g)
		}
	}
	if len(gold) != 2 {
		t.Fatalf("gold = %#v, want exactly the fixed ci-check and ci-review-bot", gold)
	}
	check, ok := byDescription["CI check failing: build - provider reported failure"]
	if !ok {
		t.Fatalf("fixed ci-check finding not ingested: %#v", gold)
	}
	if check.File != "" || check.Line != 0 {
		t.Fatalf("ci-check gold = %#v, want no fabricated location", check)
	}
	bot, ok := byDescription["greptile[bot]: possible nil dereference here"]
	if !ok {
		t.Fatalf("fixed ci-review-bot finding not ingested: %#v", gold)
	}
	if bot.File != "pkg/svc.go" || bot.Line != 42 {
		t.Fatalf("ci-review-bot gold = %#v, want file/line preserved from the persisted finding", bot)
	}
	for _, g := range gold {
		if g.Description == "runner was cancelled by the provider" {
			t.Fatalf("ingested a ci-transient finding: %#v", g)
		}
		if g.Description == "greptile[bot]: style nit the human dismissed" {
			t.Fatalf("ingested a dismissed (unselected) finding: %#v", g)
		}
	}
}

func TestCIFalseNegativesFromRun_ExcludesOverriddenAndUnselected(t *testing.T) {
	ctx := context.Background()

	// A CI step that completed only because a human approved an override is not
	// proof the findings were fixed, even though they were selected.
	_, overriddenDB, overriddenRun, _ := setupRunWithGreenReviewAndCI(t, ctx, `["ci-1","ci-2"]`, "CI checks still failing; operator approved")
	defer overriddenDB.Close()
	gold, err := CIFalseNegativesFromRun(overriddenDB, overriddenRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gold) != 0 {
		t.Fatalf("override gold = %#v, want none from a passed-with-override CI step", gold)
	}

	// Nothing selected for repair means nothing was confirmed and fixed.
	_, noSelDB, noSelRun, _ := setupRunWithGreenReviewAndCI(t, ctx, "", "")
	defer noSelDB.Close()
	gold, err = CIFalseNegativesFromRun(noSelDB, noSelRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gold) != 0 {
		t.Fatalf("no-selection gold = %#v, want none", gold)
	}
}

func TestCIFalseNegativesFromRun_IngestsPublishedRepairWithEmptySummary(t *testing.T) {
	ctx := context.Background()
	_, sourceDB, run, _ := setupRunWithCIRepairEvidence(t, ctx, `["ci-2"]`, "", true, "", true, false)
	defer sourceDB.Close()
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rounds, err := sourceDB.GetRoundsByStep(steps[len(steps)-1].ID)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["ci-2"]`
	if err := sourceDB.SetStepRoundSelection(rounds[0].ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}

	gold, err := CIFalseNegativesFromRun(sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gold) != 1 || gold[0].Description != "greptile[bot]: possible nil dereference here" {
		t.Fatalf("gold = %#v, want the user-selected repaired finding", gold)
	}
}

func TestCIFalseNegativesFromRun_RequiresLandedRepairAndPassedChecks(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name            string
		repairPublished bool
		repairSummary   string
		checksPassed    bool
		declaredNoCI    bool
	}{
		{name: "repair produced no published change", checksPassed: true},
		{name: "repair landed but checks never passed", repairPublished: true, repairSummary: "fixed and published"},
		{name: "no-CI declaration is not check evidence", repairPublished: true, repairSummary: "fixed and published", checksPassed: true, declaredNoCI: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, sourceDB, run, _ := setupRunWithCIRepairEvidence(t, ctx, `["ci-1"]`, "", tc.repairPublished, tc.repairSummary, tc.checksPassed, tc.declaredNoCI)
			defer sourceDB.Close()
			gold, err := CIFalseNegativesFromRun(sourceDB, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(gold) != 0 {
				t.Fatalf("gold = %#v, want none without both a landed repair and passed checks", gold)
			}
		})
	}
}

func TestCIFalseNegativesFromRun_TerminalPRBeforeChecksPassesNothing(t *testing.T) {
	ctx := context.Background()
	_, sourceDB, run, _ := setupRunWithCIRepairEvidence(t, ctx, `["ci-1"]`, "", true, "fixed and published", false, false)
	defer sourceDB.Close()
	if err := sourceDB.UpdateRunPRState(run.ID, "closed"); err != nil {
		t.Fatal(err)
	}
	gold, err := CIFalseNegativesFromRun(sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gold) != 0 {
		t.Fatalf("gold = %#v, want none when the PR closed before checks passed", gold)
	}
}

func TestCIFalseNegativesFromRun_RequiresCompletedRun(t *testing.T) {
	ctx := context.Background()
	_, sourceDB, run, _ := setupRunWithGreenReviewAndCI(t, ctx, `["ci-1"]`, "")
	defer sourceDB.Close()
	if err := sourceDB.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	gold, err := CIFalseNegativesFromRun(sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gold) != 0 {
		t.Fatalf("unfinished-run gold = %#v, want none", gold)
	}
}

func TestAutoIngestCIFalseNegatives_AttachesToGreenReviewAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, greenRound := setupRunWithGreenReviewAndCI(t, ctx, `["ci-1","ci-2"]`, "")
	defer sourceDB.Close()

	result, skipped, err := AutoIngestCIFalseNegatives(ctx, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if skipped {
		t.Fatal("ingest was skipped, want the fixed CI findings attached")
	}
	if result.Added != 2 || result.CaseID != run.ID+"-"+greenRound.ID {
		t.Fatalf("ingest result = %#v, want 2 FN gold on the green review case", result)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	labeled, err := store.ListCases("labeled")
	if err != nil {
		t.Fatal(err)
	}
	var ingested Case
	for _, c := range labeled {
		if c.SourceRoundID == greenRound.ID {
			ingested = c
			break
		}
	}
	if len(ingested.Labels.Findings) != 2 {
		t.Fatalf("green-review case gold = %#v, want 2 CI false negatives", ingested.Labels.Findings)
	}
	foundLocated := false
	for _, g := range ingested.Labels.Findings {
		if g.Kind != GoldFalseNegative || g.Source != goldSourceCIFalseNegative {
			t.Fatalf("attached gold = %#v, want CI false-negative", g)
		}
		if g.File == "pkg/svc.go" && g.Line == 42 {
			foundLocated = true
		}
	}
	if !foundLocated {
		t.Fatalf("review-bot gold lost its file/line: %#v", ingested.Labels.Findings)
	}

	again, skipped, err := AutoIngestCIFalseNegatives(ctx, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if skipped {
		t.Fatal("second ingest was skipped, want an idempotent no-op result")
	}
	if again.Added != 0 || again.Total != 2 {
		t.Fatalf("re-ingest = %#v, want a no-op that adds nothing", again)
	}
}
