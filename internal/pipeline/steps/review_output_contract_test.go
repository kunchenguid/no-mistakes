package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const commonReviewOutput = `{"findings":[{"title":"[P1] Preserve the result","body":"A request can lose its result.","priority":1,"confidence_score":0.98,"code_location":{"absolute_file_path":"/synthetic/handler.go","line_range":{"start":12,"end":14}}},{"title":"[P2] Keep the integration check","body":"The loader regression is no longer covered.","priority":2,"confidence_score":0.96,"code_location":{"absolute_file_path":"/synthetic/handler_test.go","line_range":{"start":22,"end":24}}}],"tested":false,"testing_summary":"Static review only.","risk_level":"high","risk_rationale":"A result can be lost.","risk_scope":"Changed handler and its callers."}`
const correctedReviewOutput = `{"findings":[{"severity":"error","description":"[P1] Preserve the result\n\nA request can lose its result.","file":"/synthetic/handler.go","line":12,"action":"ask-user","review_scope":"source"},{"severity":"warning","description":"[P2] Keep the integration check\n\nThe loader regression is no longer covered.","file":"/synthetic/handler_test.go","line":22,"action":"ask-user","review_scope":"source"}],"tested":[],"testing_summary":"Static review only.","risk_level":"high","risk_rationale":"A result can be lost.","risk_scope":"source-or-external"}`

func TestReviewStep_CorrectsCommonShapeWithoutLosingFindings(t *testing.T) {
	for _, tc := range []struct{ name, corrected, wantErr string }{
		{"preserved", correctedReviewOutput, ""},
		{"dropped", strings.Replace(correctedReviewOutput, `{"severity":"warning","description":"[P2] Keep the integration check\n\nThe loader regression is no longer covered.","file":"/synthetic/handler_test.go","line":22,"action":"ask-user","review_scope":"source"}`, ``, 1), ""},
		{"rewritten", strings.Replace(correctedReviewOutput, "A request can lose its result.", "Everything is fine.", 1), "preserve finding 1"},
		{"risk downgrade", strings.Replace(correctedReviewOutput, `"risk_level":"high"`, `"risk_level":"low"`, 1), "preserve the risk assessment"},
		{"invented tests", strings.Replace(correctedReviewOutput, `"tested":[]`, `"tested":["invented test"]`, 1), "preserve the risk assessment"},
		{"filtered", strings.Replace(correctedReviewOutput, `"review_scope":"source"`, `"review_scope":"pipeline-owned-delivery"`, 1), "preserve finding 1"},
		{"defanged", strings.Replace(correctedReviewOutput, `"action":"ask-user"`, `"action":"no-op"`, 1), "preserve finding 1"},
		{"still invalid", commonReviewOutput, "after 3 attempts"},
	} {
		if tc.name == "dropped" {
			tc.corrected = strings.Replace(tc.corrected, "},]", "}]", 1)
			tc.wantErr = "preserve all 2 findings"
		}
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				calls++
				if calls == 1 {
					return &agent.Result{Output: json.RawMessage(commonReviewOutput)}, nil
				}
				for _, want := range []string{"correction-only", "Do not use tools", "untrusted data", "A request can lose its result.", "loader regression", "severity"} {
					if !strings.Contains(opts.Prompt, want) {
						t.Errorf("correction missing %q", want)
					}
				}
				if opts.Session != nil {
					t.Error("correction must be session-free")
				}
				return &agent.Result{Output: json.RawMessage(tc.corrected)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			outcome, err := (&ReviewStep{}).Execute(sctx)
			if tc.wantErr != "" {
				if err == nil || outcome != nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("outcome=%+v err=%v want %q", outcome, err, tc.wantErr)
				}
				if calls != 3 {
					t.Fatalf("calls=%d, want bounded 3", calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings.Items) != 2 || !outcome.NeedsApproval || findings.Items[0].Severity != "error" || findings.Items[1].Severity != "warning" {
				t.Fatalf("lost review findings: %+v", outcome)
			}
			if calls != 2 {
				t.Fatalf("calls=%d, want 2", calls)
			}
		})
	}
}

func TestReviewStep_AmbiguousCorrectionInputFailsClosed(t *testing.T) {
	for _, item := range []string{
		`null`,
		`{"title":"one","body":"two","description":"three"}`,
		`{"severity":"error","action":"ask-user","review_scope":"source","title":"one","body":"two","description":"three"}`,
		`{"title":"one","body":"two","description":null}`,
		`{"body":123}`,
		`{"body":"two","title":null}`,
		`{"title":"one","body":"two","code_location":{"absolute_file_path":"a","line_range":{"start":8,"end":2}}}`,
		`{"title":"one","body":"two","code_location":null}`,
		`{"title":"one","body":"two","file":"b","code_location":{"absolute_file_path":"a","line_range":{"start":1,"end":2}}}`,
		`{"title":"one","body":"two","body":"three"}`,
	} {
		raw := `{"findings":[` + item + `],"risk_level":"high","risk_rationale":"A result can be lost.","risk_scope":"source-or-external"}`
		t.Run(item, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(raw)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			outcome, err := (&ReviewStep{}).Execute(sctx)
			if err == nil || outcome != nil {
				t.Fatalf("ambiguous input accepted: %+v %v", outcome, err)
			}
			if len(ag.calls) != 1 {
				t.Fatalf("ambiguous input sent for correction: %d calls", len(ag.calls))
			}
		})
	}
}

func TestReviewCorrectionPreservesExistingSeverity(t *testing.T) {
	original, err := reviewCorrectionBaseline([]byte(correctedReviewOutput))
	if err != nil {
		t.Fatal(err)
	}
	var corrected Findings
	if err := json.Unmarshal([]byte(strings.Replace(correctedReviewOutput, `"severity":"error"`, `"severity":"warning"`, 1)), &corrected); err != nil {
		t.Fatal(err)
	}
	if err := preserveReviewFindings(original, corrected); err == nil {
		t.Fatal("correction downgraded an existing severity")
	}
}

func TestNativeFindingsRejectDuplicateKeys(t *testing.T) {
	for _, raw := range []string{
		`{"findings":[{"severity":"error","description":"Do not lose this","action":"ask-user"}],"findings":[],"summary":"clean"}`,
		`{"findings":[{"severity":"error","description":"Do not lose this","action":"ask-user"}],"Findings":[],"summary":"clean"}`,
	} {
		var findings Findings
		if err := unmarshalRequiredFindings([]byte(raw), &findings, false); err == nil {
			t.Fatalf("native output discarded findings: %+v", findings)
		}
	}
}

func TestTestScenarioReasonConditional(t *testing.T) {
	for _, tc := range []struct {
		name, scenario, verdict string
		valid                   bool
	}{
		{"live pass", `{"name":"request","result":"pass","live":true,"evidence":"transcript"}`, "go", true},
		{"live fail", `{"name":"request","result":"fail","live":true,"evidence":"transcript"}`, "no-go", true},
		{"nonlive pass", `{"name":"request","result":"pass","live":false,"evidence":"stub"}`, "go", false},
		{"pass without evidence", `{"name":"request","result":"pass","live":true,"evidence":""}`, "go", false},
		{"untested missing", `{"name":"request","result":"untested","live":false,"evidence":""}`, "inconclusive", false},
		{"untested null", `{"name":"request","result":"untested","live":false,"evidence":"","reason":null}`, "inconclusive", false},
		{"untested blank", `{"name":"request","result":"untested","live":false,"evidence":"","reason":"  "}`, "inconclusive", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"findings":[],"summary":"synthetic","tested":["synthetic check"],"testing_summary":"synthetic observations","artifacts":[],"scenarios":[` + tc.scenario + `],"verdict":"` + tc.verdict + `"}`
			var findings Findings
			err := unmarshalRequiredTestFindings([]byte(raw), &findings)
			if (err == nil) != tc.valid {
				t.Fatalf("err=%v valid=%v", err, tc.valid)
			}
		})
	}
}
