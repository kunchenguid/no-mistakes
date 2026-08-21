package eval

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCaptureSkipsIncompleteReviewRoundAndKeepsCompletedSibling(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, firstRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sourceDB.InsertReviewStepRoundWithProvenance(steps[0].ID, 2, "auto_fix", nil, nil, run.HeadSHA, stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), firstRound.GlobalConfigYAML, firstRound.RepoConfigYAML, 0); err != nil {
		t.Fatal(err)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || cases[0].SourceRoundID != firstRound.ID {
		t.Fatalf("captured = %#v, want only the completed sibling", cases)
	}
}

func TestIngestPostPRMissWritesFalseNegativeGoldOnGreenReview(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, firstRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	clean := `{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`
	greenRound, err := sourceDB.InsertReviewStepRoundWithProvenance(steps[0].ID, 2, "initial", &clean, nil, run.HeadSHA, stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), firstRound.GlobalConfigYAML, firstRound.RepoConfigYAML, 20)
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateStepStatus(steps[0].ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	miss, err := ParsePostPRMissFinding(`{"id":"silent-wrong-set","severity":"error","file":"pkg/compute.go","line":12,"description":"returns the wrong set for a valid input"}`)
	if err != nil {
		t.Fatal(err)
	}
	result, err := IngestPostPRMiss(ctx, store, p, sourceDB, run.ID, []FindingGold{miss})
	if err != nil {
		t.Fatal(err)
	}
	if result.Added != 1 || result.Total != 1 || result.CaseID != run.ID+"-"+greenRound.ID {
		t.Fatalf("ingest result = %#v, want FN gold on green round", result)
	}

	labeled, err := store.ListCases("labeled")
	if err != nil {
		t.Fatal(err)
	}
	if len(labeled) != 2 {
		t.Fatalf("labeled cases = %d, want 2 (existing TP gold plus ingested FN)", len(labeled))
	}
	var ingested Case
	for _, c := range labeled {
		if c.SourceRoundID == greenRound.ID {
			ingested = c
			break
		}
	}
	if len(ingested.Labels.Findings) != 1 || ingested.Labels.Findings[0].Kind != GoldFalseNegative || ingested.Labels.Findings[0].Source != goldSourcePostPRMiss {
		t.Fatalf("ingested gold = %#v", ingested.Labels)
	}
	score := ScoreCandidate(ingested.Labels, `{"findings":[]}`)
	if score.FalseNegative != 1 || score.TruePositive != 0 {
		t.Fatalf("empty-review score = %#v, want FN=1 TP=0", score)
	}

	again, err := IngestPostPRMiss(ctx, store, p, sourceDB, run.ID, []FindingGold{miss})
	if err != nil {
		t.Fatal(err)
	}
	if again.Added != 0 || again.Total != 1 {
		t.Fatalf("duplicate ingest = %#v, want no-op", again)
	}

	recaptured, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var afterRecapture Case
	for _, c := range recaptured {
		if c.SourceRoundID == greenRound.ID {
			afterRecapture = c
			break
		}
	}
	if len(afterRecapture.Labels.Findings) != 1 || afterRecapture.Labels.Findings[0].Kind != GoldFalseNegative || afterRecapture.Labels.Findings[0].Source != goldSourcePostPRMiss {
		t.Fatalf("recapture gold = %#v, want ingested post-PR miss to persist", afterRecapture.Labels)
	}
}

func TestIngestPostPRMissRefusesBlockingReview(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateStepStatus(steps[0].ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	miss, err := ParsePostPRMissFinding(`{"id":"x","description":"a miss","file":"a.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = IngestPostPRMiss(ctx, store, p, sourceDB, run.ID, []FindingGold{miss})
	if !errors.Is(err, ErrReviewDidNotPassGreen) {
		t.Fatalf("error = %v, want %v", err, ErrReviewDidNotPassGreen)
	}
}

func TestIngestPostPRMissRefusesWhenLaterPassIsBlocking(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, firstRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	clean := `{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`
	if _, err := sourceDB.InsertReviewStepRoundWithProvenance(steps[0].ID, 2, "initial", &clean, nil, run.HeadSHA, stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), firstRound.GlobalConfigYAML, firstRound.RepoConfigYAML, 20); err != nil {
		t.Fatal(err)
	}
	blocking := `{"findings":[{"id":"later-bug","severity":"error","file":"pkg/compute.go","line":4,"description":"blocking finding","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	if _, err := sourceDB.InsertReviewStepRoundWithProvenance(steps[0].ID, 3, "initial", &blocking, nil, run.HeadSHA, stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), firstRound.GlobalConfigYAML, firstRound.RepoConfigYAML, 20); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateStepStatus(steps[0].ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	miss, err := ParsePostPRMissFinding(`{"id":"x","description":"a miss","file":"a.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = IngestPostPRMiss(ctx, store, p, sourceDB, run.ID, []FindingGold{miss})
	if !errors.Is(err, ErrReviewDidNotPassGreen) {
		t.Fatalf("error = %v, want %v", err, ErrReviewDidNotPassGreen)
	}
}

func TestParsePostPRMissFindingRequiresIDAndDescription(t *testing.T) {
	if _, err := ParsePostPRMissFinding(`{"file":"a.go","line":1}`); err == nil {
		t.Fatal("expected error for missing id and description")
	}
	got, err := ParsePostPRMissFinding(`{"id":"fn-1","description":"wrong label","severity":"warning","file":"a.go","line":3}`)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	if got.Kind != GoldFalseNegative || got.Source != goldSourcePostPRMiss || got.ID != "fn-1" {
		t.Fatalf("parsed = %s", raw)
	}
}

// An ingested miss's severity becomes gold and then a composition stratum, so
// free text reaches the dashboards. Accept only the review vocabulary, and
// reject an action the caller believes is meaningful rather than dropping an
// unrecognized one silently.
func TestParsePostPRMissFindingValidatesSeverityAndAction(t *testing.T) {
	for _, severity := range []string{"error", "warning", "info", "  Warning  "} {
		raw := `{"id":"fn-1","description":"a miss","severity":` + strconv.Quote(severity) + `}`
		got, err := ParsePostPRMissFinding(raw)
		if err != nil {
			t.Fatalf("severity %q: %v", severity, err)
		}
		if got.Severity != strings.ToLower(strings.TrimSpace(severity)) {
			t.Fatalf("severity %q parsed to %q, want the normalized vocabulary value", severity, got.Severity)
		}
	}

	for _, raw := range []string{
		`{"id":"fn-1","description":"a miss","severity":"blocking-correctness-defect"}`,
		`{"id":"fn-1","description":"a miss","severity":"none"}`,
		`{"id":"fn-1","description":"a miss","severity":"P1"}`,
	} {
		if _, err := ParsePostPRMissFinding(raw); err == nil {
			t.Fatalf("%s: expected an error for a severity outside the review vocabulary", raw)
		}
	}

	for _, action := range []string{"auto-fix", "ask-user", "no-op"} {
		raw := `{"id":"fn-1","description":"a miss","action":` + strconv.Quote(action) + `}`
		if _, err := ParsePostPRMissFinding(raw); err != nil {
			t.Fatalf("action %q: %v", action, err)
		}
	}
	if _, err := ParsePostPRMissFinding(`{"id":"fn-1","description":"a miss","action":"requires-human-review"}`); err == nil {
		t.Fatal("expected an error for an action outside the finding vocabulary")
	}

	// A blank severity still defaults, and a blank action stays absent.
	got, err := ParsePostPRMissFinding(`{"id":"fn-1","description":"a miss"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Severity != "error" || got.Action != "" {
		t.Fatalf("parsed = %+v, want the default error severity and no action", got)
	}
}
