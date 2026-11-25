package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The daemon entry point the IPC respond handler calls must accept an
// ignore-only fix response, and the decision must be durable: a response that
// declines every finding and selects none records the declined marker with the
// explicit empty selection the derivation reads. The fixture's sole finding is
// ask-user, so the decline is also a durable RESOLUTION: the fix round's gate
// comes back empty and the step completes without another operator decision.
// The executor-level regression proves the recording logic; this proves the
// command's own path does not refuse it earlier and that the record lands on a
// REAL parked gate.
func TestHandleRespondWithOverrides_IgnoreOnlyFixResponseIsRecorded(t *testing.T) {
	m, _, runID, _ := liveParkedGateFixture(t)

	dispositions, err := m.HandleRespondWithOverrides(runID, types.StepReview, types.ActionFix, nil, []string{"review-1"}, nil, nil, "")
	if err != nil {
		t.Fatalf("an ignore-only fix response must be accepted: %v", err)
	}
	if len(dispositions.Fixed) != 0 || strings.Join(dispositions.Ignored, ",") != "review-1" {
		t.Fatalf("dispositions = %+v, want nothing fixed and review-1 ignored", dispositions)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		recorded := false
		if steps, err := m.db.GetStepsByRun(runID); err == nil && len(steps) > 0 {
			if rounds, err := m.db.GetRoundsByStep(steps[0].ID); err == nil {
				for _, round := range rounds {
					if round.SelectionSource != nil && *round.SelectionSource == db.RoundSelectionSourceUserDeclined {
						got := ""
						if round.SelectedFindingIDs != nil {
							got = *round.SelectedFindingIDs
						}
						if got != db.DeclinedSelectionJSON {
							t.Fatalf("declined round selection = %q, want %q", got, db.DeclinedSelectionJSON)
						}
						recorded = true
					}
				}
			}
		}
		if recorded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the ignore-only response was never recorded as a declined round")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The declined ask-user finding is durably resolved: the fix round's gate
	// carries nothing and the run completes on its own - no approve needed.
	completionDeadline := time.Now().Add(5 * time.Second)
	for {
		if run, err := m.db.GetRun(runID); err == nil && run.Status == types.RunCompleted {
			break
		}
		if time.Now().After(completionDeadline) {
			t.Fatal("the durably declined gate never completed the run")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
