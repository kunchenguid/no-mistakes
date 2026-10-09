package pipeline

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The echo is a promise: a response must not be told what it recorded unless
// that decision is durable. With decision writes failing, the response is
// refused, the gate stays parked and nothing is dispatched; once writes work
// again the same response is accepted against that still-parked gate.
func TestExecutor_FixResponseRefusedWhenTheDecisionWriteFails(t *testing.T) {
	contexts := make(chan *StepContext, 4)
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			select {
			case contexts <- sctx:
			default:
			}
			return &StepOutcome{NeedsApproval: true, Findings: gateFindingsThree}, nil
		},
	}
	database, p, run, repo := setupTest(t)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	<-contexts // the parking round

	// A second connection to the same file faults only decision writes: history
	// reads and the step lifecycle keep working, which is what isolates the
	// write from everything else the response path does.
	raw, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER deny_decision BEFORE UPDATE OF selected_finding_ids ON step_rounds BEGIN SELECT RAISE(FAIL, 'injected decision write failure'); END;`); err != nil {
		t.Fatal(err)
	}

	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"R1"}, []string{"R2", "R3"}, nil, nil, ""); err == nil {
		t.Fatal("a response whose decision cannot be recorded must be refused, never echoed")
	} else if !strings.Contains(err.Error(), "record") {
		t.Fatalf("err = %v, want the failed write named", err)
	}

	// Nothing was dispatched: no further round reached the step.
	select {
	case <-contexts:
		t.Fatal("a fix round ran although no decision was recorded")
	default:
	}

	// The gate is still parked: with writes working, the same response is
	// accepted against it.
	if _, err := raw.Exec(`DROP TRIGGER deny_decision`); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"R1"}, []string{"R2", "R3"}, nil, nil, ""); err != nil {
		t.Fatalf("the still-parked gate refused the corrected response: %v", err)
	}

	approvalDeadline := time.Now().Add(5 * time.Second)
	for {
		if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err == nil {
			break
		} else if time.Now().After(approvalDeadline) {
			t.Fatalf("gate never accepted approval: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitExecutorDone(t, done)
}

// A round can be answered twice only when recovery parks it again: the first
// response's decision was recorded at park time and its dispatch never ran, so
// the second response is what reaches the fixer. It keeps the acknowledged
// content in the record and restores it into that dispatch, which is why both
// the recorded payload and the dispatch below carry the first response's
// finding, its instructions and its user-authored addition.
func TestExecutor_ReansweredRoundPreservesAcknowledgedFindingContent(t *testing.T) {
	for _, step := range []types.StepName{types.StepReview, types.StepLint} {
		t.Run(string(step), func(t *testing.T) {
			database, p, run, _ := setupTest(t)
			sr, err := database.InsertStepResult(run.ID, step)
			if err != nil {
				t.Fatal(err)
			}
			gate := gateFindingsThree
			round, err := database.InsertStepRound(sr.ID, 1, "initial", &gate, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			respond := func(ids, ignored []string, notes map[string]string, added []types.Finding) (RespondDispositions, approvalResponse) {
				t.Helper()
				exec := NewExecutor(database, p, nil, nil, nil, nil)
				exec.waiting = true
				exec.waitingStep = step
				exec.waitingStepResultID = sr.ID
				exec.waitingRoundID = round.ID
				exec.waitingFindings = gate
				exec.approvalCh = make(chan approvalResponse, 1)
				got, err := exec.RespondWithOverrides(step, types.ActionFix, ids, ignored, notes, added, "")
				if err != nil {
					t.Fatal(err)
				}
				return got, <-exec.approvalCh
			}
			read := func() *db.StepRound {
				t.Helper()
				rounds, err := database.GetRoundsByStep(sr.ID)
				if err != nil || len(rounds) != 1 {
					t.Fatalf("rounds = %v, error = %v", rounds, err)
				}
				return rounds[0]
			}
			assertContent := func(r *db.StepRound, want map[string]string) {
				t.Helper()
				if r.UserFindingsJSON == nil {
					t.Fatal("acknowledged decision has no finding payload")
				}
				findings, err := types.ParseFindingsJSON(*r.UserFindingsJSON)
				if err != nil {
					t.Fatal(err)
				}
				if len(findings.Items) != len(want) || len(selectedIDsOf(t, r)) != len(want) {
					t.Fatalf("stored decision = %+v, selected = %v", findings.Items, selectedIDsOf(t, r))
				}
				for _, item := range findings.Items {
					if description, ok := want[item.ID]; !ok || item.Description != description {
						t.Fatalf("unexpected stored identity: %+v", item)
					}
					if item.ID == "R1" && item.UserInstructions != "preserve logger format" {
						t.Fatalf("instructions lost: %+v", item)
					}
					if item.ID == "user-1" && item.UserInstructions != "keep compatibility" {
						t.Fatalf("added instructions lost: %+v", item)
					}
				}
			}
			first, _ := respond([]string{"R1"}, []string{"R2", "R3"}, map[string]string{"R1": "preserve logger format"}, []types.Finding{{Description: "repair logger", UserInstructions: "keep compatibility"}})
			if strings.Join(first.Fixed, ",") != "R1,user-1" {
				t.Fatalf("first echo = %+v", first)
			}
			assertContent(read(), map[string]string{"R1": "first", "user-1": "repair logger"})
			second, response := respond([]string{"R2"}, nil, nil, []types.Finding{{Description: "repair metrics"}})
			// The restored decision is dispatched by this response, so it is
			// echoed under fixed; R3 alone is merely kept.
			if strings.Join(second.Fixed, ",") != "R1,R2,user-1,user-2" || strings.Join(second.Kept, ",") != "R3" {
				t.Fatalf("reanswered echo = %+v", second)
			}
			assertContent(read(), map[string]string{"R1": "first", "R2": "second", "user-1": "repair logger", "user-2": "repair metrics"})
			_, merged, _, _ := normalizeFixSelection(gate, response, step == types.StepReview)
			if strings.Join(findingIDList(merged), ",") != "R1,R2,user-1,user-2" {
				t.Fatalf("dispatch = %s", merged)
			}
			dispatched, err := types.ParseFindingsJSON(merged)
			if err != nil {
				t.Fatal(err)
			}
			if dispatched.Items[0].UserInstructions != "preserve logger format" || dispatched.Items[2].UserInstructions != "keep compatibility" {
				t.Fatalf("dispatch lost the acknowledged instructions: %+v", dispatched.Items)
			}
			chosen := earlierChosenToFixIDs([]*db.StepRound{read()}, gate)
			if !chosen["R1"] || !chosen["R2"] || chosen["R3"] {
				t.Fatalf("preserved decisions = %v", chosen)
			}
		})
	}
}

func TestNormalizeFixSelection_AllocatesAgainstIgnoredGateIDs(t *testing.T) {
	gate := `{"findings":[{"id":"lint-1","description":"selected"},{"id":"lint-2","description":"declined"},{"id":"user-1","description":"also declined"}]}`
	for _, review := range []bool{false, true} {
		response := approvalResponse{
			findingIDs:    []string{"lint-1"},
			addedFindings: []types.Finding{{ID: " lint-2 ", Description: "first addition"}, {Description: "second addition"}},
		}
		_, merged, _, persisted := normalizeFixSelection(gate, response, review)
		for _, raw := range []string{merged, persisted} {
			if got := strings.Join(findingIDList(raw), ","); got != "lint-1,user-2,user-3" {
				t.Fatalf("review=%v ids=%s, want selected gate ID and two fresh IDs", review, got)
			}
		}
	}
}

// A fix response is durable before it is dispatched: persistResponseDecision
// runs while the gate is still parked, so a daemon that stops in the window
// between that write and the fix round comes back parked on the same round with
// the decision recorded and never applied. The response that re-decides that
// round is the dispatch, so it must carry the recorded selection - including
// the --add-finding item and every instruction attached to it - to the fixer
// instead of leaving it in the saved record.
func TestExecutor_RecoveredRoundRestoresTheAcceptedAddition(t *testing.T) {
	const gate = `{"findings":[` +
		`{"id":"R1","severity":"warning","description":"first"},` +
		`{"id":"R2","severity":"warning","description":"second"}],"summary":"2 findings"}`

	for _, tc := range []struct {
		name  string
		added []types.Finding
	}{
		{name: "retrying what the recovered gate shows"},
		{name: "reissuing the recorded addition", added: []types.Finding{{Description: "repair logger", UserInstructions: "keep compatibility"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			stepResult, recoveredRun := seedRecoveredReviewGate(t, database, run, gate, types.StepStatusAwaitingApproval, "")
			rounds, err := database.GetRoundsByStep(stepResult.ID)
			if err != nil || len(rounds) != 1 {
				t.Fatalf("rounds = %v, error = %v", rounds, err)
			}

			// The crashed daemon's own response: persisted, then never consumed
			// because its process stopped before the gate loop picked it up. Its
			// buffered approval channel holds the response nothing read, which is
			// exactly the durable state startup recovery finds.
			crashed := NewExecutor(database, p, nil, nil, nil, nil)
			crashed.waiting = true
			crashed.waitingStep = types.StepReview
			crashed.waitingStepResultID = stepResult.ID
			crashed.waitingRoundID = rounds[0].ID
			crashed.waitingFindings = gate
			accepted := []types.Finding{{Description: "repair logger", UserInstructions: "keep compatibility"}}
			if _, err := crashed.RespondWithOverrides(types.StepReview, types.ActionFix, []string{"R1"}, []string{"R2"}, map[string]string{"R1": "preserve logger format"}, accepted, ""); err != nil {
				t.Fatal(err)
			}

			// Recovery parks the same round again and the operator answers it,
			// either retrying the visible findings or reissuing the same command.
			dispatched := make(chan string, 4)
			step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				dispatched <- sctx.PreviousFindings
				return &StepOutcome{}, nil
			}}
			exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- exec.Resume(ctx, recoveredRun, repo, t.TempDir()) }()

			deadline := time.Now().Add(5 * time.Second)
			var respondErr error
			for time.Now().Before(deadline) {
				if respondErr = respondFixPartialWithOverrides(t, exec, types.StepReview, []string{"R1"}, nil, tc.added); respondErr == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if respondErr != nil {
				t.Fatalf("respond to recovered gate: %v", respondErr)
			}

			var merged string
			select {
			case merged = <-dispatched:
			case err := <-done:
				t.Fatalf("recovered step finished without dispatching a fix: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("recovered step never dispatched a fix")
			}
			findings, err := types.ParseFindingsJSON(merged)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(findingIDList(merged), ","); got != "R1,user-1" {
				t.Fatalf("dispatch = %s, want the selected gate finding and the recorded addition exactly once", got)
			}
			if instructions := findings.Items[0].UserInstructions; instructions != "preserve logger format" {
				t.Fatalf("accepted instructions for the gate finding = %q", instructions)
			}
			if got := findings.Items[1].UserInstructions; got != "keep compatibility" {
				t.Fatalf("accepted instructions for the addition = %q", got)
			}
			if findings.Items[1].Description != "repair logger" {
				t.Fatalf("accepted addition = %+v", findings.Items[1])
			}

			// The record keeps the same two findings under the same identity, so
			// the restored dispatch and the durable decision agree.
			recorded, err := database.GetRoundsByStep(stepResult.ID)
			if err != nil || len(recorded) == 0 {
				t.Fatalf("rounds = %v, error = %v", recorded, err)
			}
			if got := strings.Join(selectedIDsOf(t, recorded[0]), ","); got != "R1,user-1" {
				t.Fatalf("recorded selection = %s", got)
			}
			if recorded[0].UserFindingsJSON == nil || strings.Join(findingIDList(*recorded[0].UserFindingsJSON), ",") != "R1,user-1" {
				payload := "<nil>"
				if recorded[0].UserFindingsJSON != nil {
					payload = *recorded[0].UserFindingsJSON
				}
				t.Fatalf("recorded payload = %s", payload)
			}

			// Stop the recovered executor before the fixture closes its database.
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("recovered executor did not stop")
			}
		})
	}
}
