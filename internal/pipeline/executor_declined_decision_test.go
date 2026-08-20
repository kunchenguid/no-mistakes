package pipeline

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const declinedTestFindings = `{"findings":[` +
	`{"id":"journal-version-deduplication","severity":"error","file":"x.js","line":497,` +
	`"description":"dedup checks only the last event; check all prior events instead","action":"ask-user"}]}`

// runGateAndRespond drives one review gate to the given resolution and returns
// the review step's rounds.
func runGateAndRespond(t *testing.T, action types.ApprovalAction, findings string) []*db.StepRound {
	t.Helper()
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			return &StepOutcome{NeedsApproval: true, Findings: findings}, nil
		},
	}
	exec := NewExecutor(database, p, nil, nil, []Step{step, newPassStep(types.StepTest)}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	exec.Respond(types.StepReview, action, nil)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("executor timed out")
	}

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.StepName == types.StepReview {
			rounds, err := database.GetRoundsByStep(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			return rounds
		}
	}
	t.Fatal("no review step recorded")
	return nil
}

// A human who resolves a gate without picking anything to fix has still made a
// decision. Before this was recorded, that resolution left no finding-level
// trace at all, so nothing downstream could tell "the human declined every
// finding" from "there were no findings".
func TestExecutor_GateResolutionsWithoutASelectionRecordTheDecline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action types.ApprovalAction
	}{
		{"approve", types.ActionApprove},
		{"skip", types.ActionSkip},
		{"abort", types.ActionAbort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rounds := runGateAndRespond(t, tc.action, declinedTestFindings)
			if len(rounds) != 1 {
				t.Fatalf("expected 1 round, got %d", len(rounds))
			}
			r := rounds[0]
			if r.SelectionSource == nil || *r.SelectionSource != db.RoundSelectionSourceUserDeclined {
				t.Fatalf("selection_source = %v, want %q", r.SelectionSource, db.RoundSelectionSourceUserDeclined)
			}
			if r.SelectedFindingIDs == nil || *r.SelectedFindingIDs != db.DeclinedSelectionJSON {
				t.Fatalf("selected_finding_ids = %v, want %q", r.SelectedFindingIDs, db.DeclinedSelectionJSON)
			}
			// The decline must be derivable as the complement, which needs the
			// round's findings to stay intact.
			var parsed struct {
				Findings []struct {
					ID string `json:"id"`
				} `json:"findings"`
			}
			if r.FindingsJSON == nil {
				t.Fatal("round lost its findings")
			}
			if err := json.Unmarshal([]byte(*r.FindingsJSON), &parsed); err != nil {
				t.Fatal(err)
			}
			if len(parsed.Findings) != 1 || parsed.Findings[0].ID != "journal-version-deduplication" {
				t.Fatalf("unexpected retained findings: %s", *r.FindingsJSON)
			}
		})
	}
}

// Recovery must preserve the same decision semantics as the live executor.
// Otherwise a daemon restart while parked would silently reopen the original
// drift path for approve, skip, and abort.
func TestExecutor_RecoveredGateResolutionsWithoutASelectionRecordTheDecline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action types.ApprovalAction
	}{
		{"approve", types.ActionApprove},
		{"skip", types.ActionSkip},
		{"abort", types.ActionAbort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			stepResult, err := database.InsertStepResult(run.ID, types.StepTest)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.StartStep(stepResult.ID); err != nil {
				t.Fatal(err)
			}
			if err := database.SetStepFindings(stepResult.ID, declinedTestFindings); err != nil {
				t.Fatal(err)
			}
			findings := declinedTestFindings
			if _, err := database.InsertStepRound(stepResult.ID, 1, "initial", &findings, nil, 25); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 25); err != nil {
				t.Fatal(err)
			}
			if err := database.SetRunAwaitingAgent(run.ID); err != nil {
				t.Fatal(err)
			}
			run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}

			exec := NewExecutor(database, p, nil, nil, []Step{newApprovalStep(types.StepTest, declinedTestFindings)}, nil)
			done := make(chan error, 1)
			go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()

			deadline := time.Now().Add(5 * time.Second)
			for {
				if err := exec.Respond(types.StepTest, tc.action, nil); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("recovered gate never accepted the response")
				}
				time.Sleep(10 * time.Millisecond)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("recovered executor timed out")
			}

			rounds, err := database.GetRoundsByStep(stepResult.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(rounds) != 1 {
				t.Fatalf("expected 1 round, got %d", len(rounds))
			}
			if rounds[0].SelectionSource == nil || *rounds[0].SelectionSource != db.RoundSelectionSourceUserDeclined {
				t.Fatalf("selection_source = %v, want %q", rounds[0].SelectionSource, db.RoundSelectionSourceUserDeclined)
			}
			if rounds[0].SelectedFindingIDs == nil || *rounds[0].SelectedFindingIDs != db.DeclinedSelectionJSON {
				t.Fatalf("selected_finding_ids = %v, want %q", rounds[0].SelectedFindingIDs, db.DeclinedSelectionJSON)
			}
		})
	}
}

// A round that produced nothing to decide about must not be labelled a
// decision, or every clean step would render as a decline.
func TestExecutor_GateResolutionWithNoFindingsRecordsNoDecision(t *testing.T) {
	rounds := runGateAndRespond(t, types.ActionApprove, `{"findings":[]}`)
	if len(rounds) != 1 {
		t.Fatalf("expected 1 round, got %d", len(rounds))
	}
	if rounds[0].SelectionSource != nil {
		t.Fatalf("selection_source = %q, want no recorded decision", *rounds[0].SelectionSource)
	}
}

// Choosing to fix is still recorded as a selection, not as a decline.
func TestExecutor_FixResolutionStillRecordsAUserSelection(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	calls := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			calls++
			if calls == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: declinedTestFindings}, nil
			}
			return &StepOutcome{NeedsApproval: false, ExitCode: 0}, nil
		},
	}
	exec := NewExecutor(database, p, nil, nil, []Step{step, newPassStep(types.StepTest)}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	exec.Respond(types.StepReview, types.ActionFix, []string{"journal-version-deduplication"})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("executor timed out")
	}

	steps, _ := database.GetStepsByRun(run.ID)
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) == 0 {
		t.Fatal("no rounds")
	}
	r := rounds[0]
	if r.SelectionSource == nil || *r.SelectionSource != db.RoundSelectionSourceUser {
		t.Fatalf("selection_source = %v, want %q", r.SelectionSource, db.RoundSelectionSourceUser)
	}
	if r.SelectedFindingIDs == nil || *r.SelectedFindingIDs == db.DeclinedSelectionJSON {
		t.Fatalf("selected_finding_ids = %v, want the selected id", r.SelectedFindingIDs)
	}
}
