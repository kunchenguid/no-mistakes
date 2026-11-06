package db

import (
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const declinedFindings = `{"findings":[{"id":"keep-a","severity":"error","description":"switch to B","action":"ask-user"}]}`

func seedRound(t *testing.T, d *DB, repoID, branch string, stepName types.StepName) (*Run, *StepResult, *StepRound) {
	t.Helper()
	run, err := d.InsertRun(repoID, branch, "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	sr, err := d.InsertStepResult(run.ID, stepName)
	if err != nil {
		t.Fatal(err)
	}
	findings := declinedFindings
	round, err := d.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	return run, sr, round
}

func TestSetStepRoundDeclined_RecordsAnExplicitEmptySelection(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	_, sr, round := seedRound(t, d, repo.ID, "feature", types.StepReview)

	if err := d.SetStepRoundDeclined(round.ID); err != nil {
		t.Fatal(err)
	}

	rounds, err := d.GetRoundsByStep(sr.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("rounds: %v %d", err, len(rounds))
	}
	if rounds[0].SelectionSource == nil || *rounds[0].SelectionSource != RoundSelectionSourceUserDeclined {
		t.Fatalf("selection_source = %v, want %q", rounds[0].SelectionSource, RoundSelectionSourceUserDeclined)
	}
	if rounds[0].SelectedFindingIDs == nil || *rounds[0].SelectedFindingIDs != DeclinedSelectionJSON {
		t.Fatalf("selected_finding_ids = %v, want %q", rounds[0].SelectedFindingIDs, DeclinedSelectionJSON)
	}
}

// A decline records that nothing was selected. It must never erase a selection
// that was already recorded, or the decline would destroy the decision it is
// meant to preserve.
func TestSetStepRoundDeclined_NeverOverwritesARecordedSelection(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	_, sr, round := seedRound(t, d, repo.ID, "feature", types.StepReview)

	selected := `["keep-a"]`
	if err := d.SetStepRoundSelection(round.ID, &selected, RoundSelectionSourceAutoFix); err != nil {
		t.Fatal(err)
	}
	if err := d.SetStepRoundDeclined(round.ID, types.ResponseOrigin("auto_yes")); err != nil {
		t.Fatal(err)
	}

	rounds, err := d.GetRoundsByStep(sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rounds[0].SelectionSource == nil || *rounds[0].SelectionSource != RoundSelectionSourceAutoFix {
		t.Fatalf("selection_source = %v, want the original %q", rounds[0].SelectionSource, RoundSelectionSourceAutoFix)
	}
	if rounds[0].SelectedFindingIDs == nil || *rounds[0].SelectedFindingIDs != selected {
		t.Fatalf("selected_finding_ids = %v, want the original %q", rounds[0].SelectedFindingIDs, selected)
	}
	if rounds[0].ResponseOrigin != types.ResponseOrigin("unknown") {
		t.Fatalf("response_origin = %q, want the original %q", rounds[0].ResponseOrigin, "unknown")
	}
}

// An explicit empty JSON array is a recorded decision; an empty string is not.
// Readers derive the declined set by subtraction, so the two must not collapse.
func TestSetStepRoundSelection_DistinguishesEmptyArrayFromNoDecision(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	_, sr, round := seedRound(t, d, repo.ID, "feature", types.StepReview)

	empty := DeclinedSelectionJSON
	if err := d.SetStepRoundSelection(round.ID, &empty, RoundSelectionSourceUserDeclined); err != nil {
		t.Fatal(err)
	}
	rounds, _ := d.GetRoundsByStep(sr.ID)
	if rounds[0].SelectionSource == nil {
		t.Fatal("an empty JSON array must keep its selection source")
	}

	blank := ""
	if err := d.SetStepRoundSelection(round.ID, &blank, RoundSelectionSourceUserDeclined); err != nil {
		t.Fatal(err)
	}
	rounds, _ = d.GetRoundsByStep(sr.ID)
	if rounds[0].SelectionSource != nil {
		t.Fatalf("an empty string must clear the source, got %q", *rounds[0].SelectionSource)
	}
}

func TestGetBranchDecisionRounds_ScopesToOtherRunsOnTheSameBranch(t *testing.T) {
	d := openTestDB(t)
	repoA, err := d.InsertRepo(t.TempDir(), "https://example.invalid/a", "main")
	if err != nil {
		t.Fatal(err)
	}
	repoB, err := d.InsertRepo(t.TempDir(), "https://example.invalid/b", "main")
	if err != nil {
		t.Fatal(err)
	}

	decline := func(repoID, branch string, step types.StepName) *Run {
		run, _, round := seedRound(t, d, repoID, branch, step)
		if err := d.SetStepRoundDeclined(round.ID); err != nil {
			t.Fatal(err)
		}
		return run
	}

	wanted := decline(repoA.ID, "feature", types.StepReview)
	current := decline(repoA.ID, "feature", types.StepReview)
	decline(repoA.ID, "other-branch", types.StepReview)
	decline(repoB.ID, "feature", types.StepReview)
	// A round with no recorded human decision must not surface.
	seedRound(t, d, repoA.ID, "feature", types.StepTest)

	got, truncated, err := d.GetBranchDecisionRounds(repoA.ID, "feature", current.ID, MaxBranchDecisionRounds)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("one matching decision must not report truncation")
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly the other same-branch decision, got %d", len(got))
	}
	if got[0].RunID != wanted.ID {
		t.Fatalf("run id = %q, want %q", got[0].RunID, wanted.ID)
	}
	if got[0].StepName != types.StepReview {
		t.Fatalf("step name = %q, want %q", got[0].StepName, types.StepReview)
	}
	if got[0].Round == nil || got[0].Round.FindingsJSON == nil {
		t.Fatal("decision round lost its findings")
	}
}

// An auto-fix selection is a filter's choice, not a human's, so it must not be
// served as a prior decision.
func TestGetBranchDecisionRounds_ExcludesAutoFixSelections(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	_, _, round := seedRound(t, d, repo.ID, "feature", types.StepReview)
	selected := `["keep-a"]`
	if err := d.SetStepRoundSelection(round.ID, &selected, RoundSelectionSourceAutoFix); err != nil {
		t.Fatal(err)
	}
	current, err := d.InsertRun(repo.ID, "feature", "head2", "base")
	if err != nil {
		t.Fatal(err)
	}

	got, truncated, err := d.GetBranchDecisionRounds(repo.ID, "feature", current.ID, MaxBranchDecisionRounds)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("an empty result must not report truncation")
	}
	if len(got) != 0 {
		t.Fatalf("expected no human decisions, got %d", len(got))
	}
}

func TestGetBranchDecisionRounds_BoundsTheHistory(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		_, _, round := seedRound(t, d, repo.ID, "feature", types.StepReview)
		if err := d.SetStepRoundDeclined(round.ID); err != nil {
			t.Fatal(err)
		}
	}
	current, err := d.InsertRun(repo.ID, "feature", "head-current", "base")
	if err != nil {
		t.Fatal(err)
	}

	got, truncated, err := d.GetBranchDecisionRounds(repo.ID, "feature", current.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected the bound to apply, got %d", len(got))
	}
	if !truncated {
		t.Fatal("expected older matching decisions to report truncation")
	}
}

// An ignore-only fix response records that nothing was selected. Recovery can
// re-park a round that already carries an external disposition, so the write
// must never replace that source nor erase the operator's recorded reason.
func TestSetStepRoundIgnored_KeepsAnExternalDispositionAndItsReason(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	_, sr, round := seedRound(t, d, repo.ID, "feature", types.StepReview)

	empty := DeclinedSelectionJSON
	reason := `{"findings":[{"id":"keep-a","severity":"error","description":"switch to B","action":"ask-user","user_instructions":"grant verified on the machine"}]}`
	if err := d.SetStepRoundUserDecision(round.ID, &empty, RoundSelectionSourceExternal, &reason); err != nil {
		t.Fatal(err)
	}
	if err := d.SetStepRoundIgnored(round.ID, types.ResponseOrigin("interactive")); !errors.Is(err, ErrStepRoundDecisionRecorded) {
		t.Fatalf("error = %v, want ErrStepRoundDecisionRecorded so the caller can refuse the response", err)
	}

	rounds, err := d.GetRoundsByStep(sr.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("rounds: %v %d", err, len(rounds))
	}
	if rounds[0].SelectionSource == nil || *rounds[0].SelectionSource != RoundSelectionSourceExternal {
		t.Fatalf("selection_source = %v, want the original %q", rounds[0].SelectionSource, RoundSelectionSourceExternal)
	}
	if rounds[0].UserFindingsJSON == nil || *rounds[0].UserFindingsJSON != reason {
		t.Fatalf("user_findings_json = %v, want the recorded reason preserved", rounds[0].UserFindingsJSON)
	}
}

// An ignore-only fix response on an undecided round records its own source, so
// it stays distinguishable from an approve/skip/abort decline.
func TestSetStepRoundIgnored_RecordsItsOwnSource(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	_, sr, round := seedRound(t, d, repo.ID, "feature", types.StepReview)

	if err := d.SetStepRoundIgnored(round.ID); err != nil {
		t.Fatal(err)
	}

	rounds, err := d.GetRoundsByStep(sr.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("rounds: %v %d", err, len(rounds))
	}
	if rounds[0].SelectionSource == nil || *rounds[0].SelectionSource != RoundSelectionSourceUserIgnored {
		t.Fatalf("selection_source = %v, want %q", rounds[0].SelectionSource, RoundSelectionSourceUserIgnored)
	}
	if rounds[0].SelectedFindingIDs == nil || *rounds[0].SelectedFindingIDs != DeclinedSelectionJSON {
		t.Fatalf("selected_finding_ids = %v, want %q", rounds[0].SelectedFindingIDs, DeclinedSelectionJSON)
	}
}

// Recovery re-parks a round that recorded an approve, skip, or abort decline.
// An ignore-only fix response answering that gate is accepted and refreshes
// the caller origin, but the recorded decline stays a decline: promoting it to
// the sticky ignore source would suppress findings on the strength of a
// response the operator never gave.
func TestSetStepRoundIgnored_KeepsARecordedDecline(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	_, sr, round := seedRound(t, d, repo.ID, "feature", types.StepReview)

	if err := d.SetStepRoundDeclined(round.ID, types.ResponseOrigin("interactive")); err != nil {
		t.Fatal(err)
	}
	if err := d.SetStepRoundIgnored(round.ID, types.ResponseOrigin("auto_yes")); err != nil {
		t.Fatal(err)
	}

	rounds, err := d.GetRoundsByStep(sr.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("rounds: %v %d", err, len(rounds))
	}
	if rounds[0].SelectionSource == nil || *rounds[0].SelectionSource != RoundSelectionSourceUserDeclined {
		t.Fatalf("selection_source = %v, want the recorded %q", rounds[0].SelectionSource, RoundSelectionSourceUserDeclined)
	}
	if rounds[0].ResponseOrigin != types.ResponseOrigin("auto_yes") {
		t.Fatalf("response_origin = %q, want %q", rounds[0].ResponseOrigin, "auto_yes")
	}
	if rounds[0].SelectedFindingIDs == nil || *rounds[0].SelectedFindingIDs != DeclinedSelectionJSON {
		t.Fatalf("selected_finding_ids = %v, want %q", rounds[0].SelectedFindingIDs, DeclinedSelectionJSON)
	}
}

// A user selection and its dispatched payload are never replaced by an
// ignore-only response, and the caller is told nothing was stored.
func TestSetStepRoundIgnored_KeepsAUserSelection(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	_, sr, round := seedRound(t, d, repo.ID, "feature", types.StepReview)

	selected := `["keep-a"]`
	payload := declinedFindings
	if err := d.SetStepRoundUserDecision(round.ID, &selected, RoundSelectionSourceUser, &payload); err != nil {
		t.Fatal(err)
	}
	if err := d.SetStepRoundIgnored(round.ID); !errors.Is(err, ErrStepRoundDecisionRecorded) {
		t.Fatalf("error = %v, want ErrStepRoundDecisionRecorded", err)
	}

	rounds, err := d.GetRoundsByStep(sr.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("rounds: %v %d", err, len(rounds))
	}
	if rounds[0].SelectionSource == nil || *rounds[0].SelectionSource != RoundSelectionSourceUser {
		t.Fatalf("selection_source = %v, want the original %q", rounds[0].SelectionSource, RoundSelectionSourceUser)
	}
	if rounds[0].SelectedFindingIDs == nil || *rounds[0].SelectedFindingIDs != selected {
		t.Fatalf("selected_finding_ids = %v, want the original %q", rounds[0].SelectedFindingIDs, selected)
	}
}
