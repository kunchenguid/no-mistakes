package pipeline

import (
	"encoding/json"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func gateFindings(actions ...string) string {
	items := make([]types.Finding, len(actions))
	for i, action := range actions {
		items[i] = types.Finding{ID: string(rune('a' + i)), Severity: "error", Description: string(rune('a' + i)), Action: action}
	}
	data, _ := json.Marshal(types.Findings{Items: items})
	return string(data)
}

func TestExecutor_GateAutoFixBudget(t *testing.T) {
	auto := types.ActionAutoFix
	ask := types.ActionAskUser
	for _, tc := range []struct {
		name       string
		budget     int
		outputs    []string
		wantCalls  int
		wantStatus types.StepStatus
	}{
		{"default zero", 0, []string{gateFindings(auto)}, 1, types.StepStatusAwaitingApproval},
		{"shrinks then passes", 3, []string{gateFindings(auto, auto), gateFindings(auto), ""}, 3, types.StepStatusCompleted},
		{"budget exhausted", 1, []string{gateFindings(auto, auto), gateFindings(auto)}, 2, types.StepStatusFixReview},
		{"no progress", 3, []string{gateFindings(auto), gateFindings(auto)}, 2, types.StepStatusFixReview},
		{"grows", 3, []string{gateFindings(auto), gateFindings(auto, auto)}, 2, types.StepStatusFixReview},
		{"ask-user alone", 3, []string{gateFindings(ask)}, 1, types.StepStatusAwaitingApproval},
		{"mixed actions", 3, []string{gateFindings(auto, ask)}, 1, types.StepStatusAwaitingApproval},
		{"unclassified", 3, []string{gateFindings(auto, "")}, 1, types.StepStatusAwaitingApproval},
		{"ask-user after repair", 3, []string{gateFindings(auto, auto), gateFindings(ask)}, 2, types.StepStatusFixReview},
		{"no-op", 3, []string{gateFindings(types.ActionNoOp)}, 1, types.StepStatusAwaitingApproval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			name := types.CustomGateStepName(types.StepTest, "mutation")
			calls := 0
			step := &adaptiveCallStep{name: name, fn: func(sctx *StepContext) (*StepOutcome, error) {
				if calls >= len(tc.outputs) {
					t.Errorf("unexpected repair %d", calls)
					return &StepOutcome{}, nil
				}
				output := tc.outputs[calls]
				calls++
				if calls > 1 && (!sctx.Fixing || sctx.PreviousFindings == "") {
					t.Error("repair lacks findings or fix context")
				}
				return &StepOutcome{NeedsApproval: output != "", Findings: output}, nil
			}}
			cfg := &config.Config{AutoFix: config.AutoFix{Gates: map[string]int{"mutation": tc.budget}}}
			exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
			done, _ := startExecutor(t, exec, run, repo, t.TempDir())
			waitForStepStatus(t, database, run.ID, name, tc.wantStatus)
			if calls != tc.wantCalls {
				t.Errorf("calls=%d, want %d", calls, tc.wantCalls)
			}
			results, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			rounds, err := database.GetRoundsByStep(results[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			gotBudget := 0
			if results[0].AutoFixLimit != nil {
				gotBudget = *results[0].AutoFixLimit
			}
			if gotBudget != tc.budget {
				t.Errorf("recorded budget=%d, want %d", gotBudget, tc.budget)
			}
			for i, round := range rounds {
				if i < tc.wantCalls-1 && (round.SelectionSource == nil || *round.SelectionSource != db.RoundSelectionSourceAutoFix) {
					t.Error("automatic selection not recorded")
				}
			}
			if tc.wantStatus != types.StepStatusCompleted {
				exec.Respond(name, types.ActionApprove, nil)
			}
			waitExecutorDone(t, done)
		})
	}
}

func TestGateAutoFixEligibility(t *testing.T) {
	auto := types.ActionAutoFix
	for _, tc := range []struct {
		name, current, previous, deferred string
		fixing, want                      bool
	}{
		{"initial", gateFindings(auto), "", "", false, true},
		{"deferred counts", gateFindings(auto), gateFindings(auto), gateFindings(types.ActionNoOp), true, true},
		{"same count different finding", `{"findings":[{"description":"different","action":"auto-fix"}]}`, gateFindings(auto), "", true, false},
		{"malformed current", "bad", "", "", false, false},
		{"malformed previous", gateFindings(auto), "bad", "", true, false},
		{"malformed deferred", gateFindings(auto), gateFindings(auto, auto), "bad", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gateAutoFixEligible(tc.current, tc.previous, tc.deferred, tc.fixing); got != tc.want {
				t.Fatalf("eligible=%v, want %v", got, tc.want)
			}
		})
	}
}
