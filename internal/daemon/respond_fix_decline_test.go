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
// explicit empty selection the derivation reads. The executor-level regression
// proves the recording logic; this proves the command's own path does not
// refuse it earlier and that the record lands on a REAL parked gate.
func TestHandleRespondWithOverrides_IgnoreOnlyFixResponseIsRecorded(t *testing.T) {
	m, _, runID, exec := liveParkedGateFixture(t)

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

	// End the run so the fixture's cleanup observes the executor finishing.
	approvalDeadline := time.Now().Add(5 * time.Second)
	for {
		if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err == nil {
			break
		} else if time.Now().After(approvalDeadline) {
			t.Fatalf("gate never accepted approval: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
