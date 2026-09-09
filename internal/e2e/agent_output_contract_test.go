//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Synthetic validator inputs only: these simulate agent reports, not evidence
// that the fixture application's scenarios were actually driven.
func TestAgentOutputContractJourney(t *testing.T) {
	const review = `{"findings":[{"title":"[P1] Preserve the result","body":"A request can lose its result.","priority":1,"confidence_score":0.98,"code_location":{"absolute_file_path":"/synthetic/handler.go","line_range":{"start":12,"end":14}}}],"tested":false,"testing_summary":"Static review only.","risk_level":"high","risk_rationale":"A result can be lost.","risk_scope":"Changed handler and its callers."}`
	const corrected = `{"findings":[{"severity":"error","description":"[P1] Preserve the result\n\nA request can lose its result.","file":"/synthetic/handler.go","line":12,"action":"ask-user","review_scope":"source"}],"tested":[],"testing_summary":"Static review only.","risk_level":"high","risk_rationale":"A result can be lost.","risk_scope":"source-or-external"}`
	const tested = `{"findings":[],"summary":"Synthetic validation","tested":["synthetic check"],"testing_summary":"Synthetic observations.","artifacts":[],"scenarios":[{"name":"Submit a request","result":"pass","live":true,"evidence":"synthetic transcript"},{"name":"Read remote result","result":"untested","live":false,"evidence":"","reason":"Sandbox credential unavailable; provide it to run."}],"verdict":"go"}`
	const clean = `{"findings":[],"summary":"Synthetic clean check","tested":["synthetic check"],"testing_summary":"Synthetic observations.","artifacts":[],"scenarios":[{"name":"Synthetic request","result":"pass","live":true,"evidence":"synthetic transcript"}],"verdict":"go","risk_level":"low","risk_rationale":"Synthetic clean review","risk_scope":"source-or-external"}`
	scenario := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(scenario, []byte("actions: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Antigravity and pi share finalizeTextResult and the exact schema parser.
	// Replay a native stream through the real binary, not a mock step result.
	h := NewHarness(t, SetupOpts{Agent: "antigravity", Scenario: scenario})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	for _, tc := range []struct {
		name, response, correction, wantError string
		step                                  types.StepName
	}{
		{"review-preserved", review, corrected, "", types.StepReview},
		{"review-dropped", review, clean, "preserve all 1 findings", types.StepReview},
		{"test-pass", tested, tested, "", types.StepTest},
		{"test-schema-corrected", strings.Replace(tested, `"summary":"Synthetic validation",`, "", 1), tested, "", types.StepTest},
		{"test-nonlive", strings.Replace(tested, `"live":true`, `"live":false`, 1), strings.Replace(tested, `"live":true`, `"live":false`, 1), `result "pass" but live=false`, types.StepTest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			priorInvocations := len(h.AgentInvocations())
			match := "Review the code changes and return structured findings"
			if tc.step == types.StepTest {
				match = "You are validating a code change by driving the product itself."
			}
			correctionMatch := "were REJECTED"
			if tc.name == "test-schema-corrected" {
				// This observation is beyond the finalizer's 200-character error
				// snippet. Recovery must receive the whole rejected payload.
				correctionMatch = "Sandbox credential unavailable; provide it to run."
			}
			body := "actions:\n  - match: '" + correctionMatch + "'\n    structured_raw: '" + tc.correction + "'\n  - match: 'were REJECTED'\n    structured_raw: '{}'\n  - match: '" + match + "'\n    structured_raw: '" + tc.response + "'\n  - match: ''\n    structured_raw: '" + clean + "'\n"
			if err := os.WriteFile(scenario, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			branch := "contract-" + tc.name
			h.CommitChange(branch, "change.txt", "synthetic change\n", "exercise output contract")
			h.PushToGate(branch)
			if tc.step == types.StepReview && tc.wantError == "" {
				run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
				step, _ := findStep(run.Steps, types.StepReview)
				if step.FindingsJSON == nil {
					t.Fatal("missing persisted review")
				}
				findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
				if err != nil || len(findings.Items) != 1 || findings.Items[0].Description != "[P1] Preserve the result\n\nA request can lose its result." || findings.Items[0].Action != types.ActionAskUser || findings.Items[0].Severity != "error" {
					t.Fatalf("lost corrected finding: %+v %v", findings, err)
				}
				h.Respond(run.ID, types.StepReview, types.ActionAbort)
				h.WaitForRun(branch, 60*time.Second)
				return
			}
			run := h.WaitForRun(branch, 60*time.Second)
			if tc.wantError != "" {
				if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, tc.wantError) || !strings.Contains(*run.Error, "after 3 attempts") {
					t.Fatalf("status=%s error=%v, want bounded failure: %s", run.Status, deref(run.Error), tc.wantError)
				}
				step, _ := findStep(run.Steps, tc.step)
				if step.FindingsJSON != nil {
					t.Fatalf("rejected output persisted as findings: %s", *step.FindingsJSON)
				}
				return
			}
			step, _ := findStep(run.Steps, types.StepTest)
			if step.Status != types.StepStatusCompleted || step.FindingsJSON == nil {
				t.Fatalf("test status=%s error=%v", step.Status, deref(run.Error))
			}
			findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
			if err != nil || len(findings.Scenarios) != 2 || findings.Verdict != "go" || findings.Scenarios[0].Reason != "" || findings.Scenarios[1].Reason == "" {
				t.Fatalf("lost test evidence: %+v %v", findings, err)
			}
			corrections := 0
			for _, invocation := range h.AgentInvocations()[priorInvocations:] {
				if strings.Contains(invocation.Prompt, "were REJECTED") {
					corrections++
				}
			}
			wantCorrections := 0
			if tc.name == "test-schema-corrected" {
				wantCorrections = 1
			}
			if corrections != wantCorrections {
				t.Fatalf("corrections=%d, want %d", corrections, wantCorrections)
			}
		})
	}
}
