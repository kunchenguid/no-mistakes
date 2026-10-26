package pipeline

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// An agent driver's --instructions scope one fix round. They are recorded and
// dispatched as driver_instructions, never as user_instructions, so no later
// prompt can present them as the user's own decision - including when
// recovery re-parks the round and a second response re-decides it.
func TestExecutor_DriverInstructionsStayApartFromUserInstructions(t *testing.T) {
	database, p, run, _ := setupTest(t)
	sr, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	gate := gateFindingsThree
	round, err := database.InsertStepRound(sr.ID, 1, "initial", &gate, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	respond := func(ids, ignored []string, instructions *FixInstructions) approvalResponse {
		t.Helper()
		exec := NewExecutor(database, p, nil, nil, nil, nil)
		exec.waiting = true
		exec.waitingStep = types.StepReview
		exec.waitingStepResultID = sr.ID
		exec.waitingRoundID = round.ID
		exec.waitingFindings = gate
		exec.approvalCh = make(chan approvalResponse, 1)
		if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, ids, ignored, instructions, nil, ""); err != nil {
			t.Fatal(err)
		}
		return <-exec.approvalCh
	}
	assertDriverOnly := func(label, raw string) {
		t.Helper()
		findings, err := types.ParseFindingsJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range findings.Items {
			if item.ID != "R1" {
				continue
			}
			if item.DriverInstructions != "only touch a.go" || item.UserInstructions != "" {
				t.Fatalf("%s R1 = %+v, want the note as driver_instructions only", label, item)
			}
			return
		}
		t.Fatalf("%s has no R1: %s", label, raw)
	}

	driver := &FixInstructions{ByFinding: map[string]string{"R1": "only touch a.go"}, FromDriver: true}
	response := respond([]string{"R1"}, []string{"R2", "R3"}, driver)
	_, merged, _, _ := normalizeFixSelection(gate, response, true)
	assertDriverOnly("first dispatch", merged)
	rounds, err := database.GetRoundsByStep(sr.ID)
	if err != nil || len(rounds) != 1 || rounds[0].UserFindingsJSON == nil {
		t.Fatalf("rounds = %v, error = %v", rounds, err)
	}
	assertDriverOnly("recorded decision", *rounds[0].UserFindingsJSON)

	// Recovery re-parks the round; the re-deciding response restores the
	// recorded guidance under the author it was recorded with.
	response = respond([]string{"R2"}, nil, nil)
	_, merged, _, _ = normalizeFixSelection(gate, response, true)
	assertDriverOnly("restored dispatch", merged)
}
