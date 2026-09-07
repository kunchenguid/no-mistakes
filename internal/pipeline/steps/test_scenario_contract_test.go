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

// TestTestStep_PromptDerivesScenariosAndMarksLive pins the live-validation
// prompt contract: the step asks for named scenarios driven against the real
// product, an explicit live marking that a unit test cannot claim, an honest
// untested result with a reason instead of a guessed pass, and a verdict. The
// pre-contract framing that let a green unit-test run stand in for driving the
// product must be gone.
func TestTestStep_PromptDerivesScenariosAndMarksLive(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Show users a success screen after checkout"

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	prompt := ag.calls[0].Prompt
	for _, want := range []string{
		// Scenario derivation, not test selection.
		"Derive the scenarios this change must satisfy, then run each one against the real running product",
		"Turn that intent into a short list of named scenarios",
		"one concrete thing an end user does and one observable result that proves it",
		"add an adversarial scenario that actively tries to break it",
		// Live is a claim about what actually ran.
		"drive each scenario end-to-end against that running product",
		`Mark a scenario "live": true ONLY when you drove it against the real product in this run`,
		"A unit test, a stub, a mock, a recorded fixture, or reading the code is NOT live",
		// Untested is honest and cheap; a guessed pass is not.
		`return it with result "untested" and a reason naming the specific tool, credential, permission, or authority`,
		"Never guess a pass",
		"an honest \"untested\" costs nothing and a guessed \"pass\" costs everything",
		"reported as an untested scenario with its reason, NOT as a finding",
		// The verdict and what it does.
		`Return a "verdict"`,
		`A "no-go" verdict parks this step for a decision`,
		"Untested scenarios are listed on the pull request and do not park by themselves",
		// The targeted-validation boundary survives the rewrite.
		"Do NOT run the complete repository test suite",
		"remote CI owns broad regression and remains mandatory before a PR is ready",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected evidence prompt to contain %q\nprompt:\n%s", want, prompt)
		}
	}
	for _, forbidden := range []string{
		"run the smallest relevant tests yourself",
		"Look for existing tests that would generate sufficient evidence",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("evidence prompt still carries pre-contract framing %q", forbidden)
		}
	}
}

func TestTestStep_PromptIncludesOnlyConfiguredTrustedRunbook(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		instructions string
		wantRunbook  bool
	}{
		{name: "none configured"},
		{name: "configured", instructions: "Start the app with `make dev` and drive checkout.", wantRunbook: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config.Test.Instructions = tc.instructions

			if _, err := (&TestStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			prompt := ag.calls[0].Prompt
			if got := strings.Contains(prompt, "Repository live-validation runbook (trusted, from the default branch):"); got != tc.wantRunbook {
				t.Fatalf("runbook section present = %v, want %v\nprompt:\n%s", got, tc.wantRunbook, prompt)
			}
			if tc.wantRunbook && !strings.Contains(prompt, tc.instructions) {
				t.Fatalf("prompt omitted configured runbook:\n%s", prompt)
			}
		})
	}
}

func TestTestStep_FailingBaselineStillRunsEvidenceTurn(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	testCmd := "printf 'baseline broke'; exit 7"
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: testCmd})

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("evidence agent calls = %d, want 1", calls)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || !outcome.AutoFixable || outcome.ExitCode != 7 {
		t.Fatalf("outcome = %+v, want blocking auto-fixable baseline failure", outcome)
	}
	if findings.Verdict != types.TestVerdictGo || len(findings.Scenarios) != 1 {
		t.Fatalf("evidence contract was not retained: %+v", findings)
	}
	if len(findings.Tested) < 2 || findings.Tested[0] != testCmd {
		t.Fatalf("tested = %+v, want baseline followed by evidence checks", findings.Tested)
	}
	if len(findings.Items) == 0 || !strings.Contains(findings.Items[0].Description, "tests failed with exit code 7") {
		t.Fatalf("baseline finding missing from %+v", findings.Items)
	}
}

const passingScenarioFindingsJSON = `{
  "findings": [],
  "summary": "",
  "tested": ["npm run e2e -- checkout"],
  "testing_summary": "drove checkout end to end",
  "artifacts": [],
  "scenarios": [{"name":"user reaches the success screen","result":"pass","live":true,"evidence":"checkout.png","reason":""}],
  "verdict": "go"
}`

// TestTestStep_VerdictPolicy proves captain's call C2 = a end to end: a no-go
// verdict parks the step with a blocking finding, an untested scenario passes
// through without parking, and a go verdict adds nothing. All three keep the
// scenario record on the step so the PR can render it.
func TestTestStep_VerdictPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		output            string
		wantApproval      bool
		wantDescription   string
		wantScenarioCount int
	}{
		{
			name:              "go passes through",
			output:            passingScenarioFindingsJSON,
			wantApproval:      false,
			wantScenarioCount: 1,
		},
		{
			name: "untested is listed without parking",
			output: `{"findings":[],"summary":"","tested":["manual check"],"testing_summary":"partly driven","artifacts":[],
				"scenarios":[
					{"name":"user reaches the success screen","result":"pass","live":true,"evidence":"checkout.png","reason":""},
					{"name":"payment declines are shown","result":"untested","live":false,"evidence":"","reason":"no card sandbox credential on this machine"}
				],"verdict":"go"}`,
			wantApproval:      false,
			wantScenarioCount: 2,
		},
		{
			name: "no-go parks",
			output: `{"findings":[],"summary":"","tested":["npm run e2e -- checkout"],"testing_summary":"checkout broke","artifacts":[],
				"scenarios":[{"name":"user reaches the success screen","result":"fail","live":true,"evidence":"checkout.png","reason":""}],
				"verdict":"no-go"}`,
			wantApproval:      true,
			wantDescription:   "live validation verdict: no-go",
			wantScenarioCount: 1,
		},
		{
			name: "inconclusive parks for a human",
			output: `{"findings":[],"summary":"","tested":["read the diff"],"testing_summary":"nothing could be driven","artifacts":[],
				"scenarios":[{"name":"user reaches the success screen","result":"untested","live":false,"evidence":"","reason":"no browser on this machine"}],
				"verdict":"inconclusive"}`,
			wantApproval:      true,
			wantDescription:   "live validation verdict: inconclusive",
			wantScenarioCount: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			output := tc.output
			ag := &mockAgent{
				name: "test",
				runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					return &agent.Result{Output: json.RawMessage(output)}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.UserIntent = "Show users a success screen after checkout"

			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.NeedsApproval != tc.wantApproval {
				t.Fatalf("NeedsApproval = %v, want %v (findings: %s)", outcome.NeedsApproval, tc.wantApproval, outcome.Findings)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings.Scenarios) != tc.wantScenarioCount {
				t.Fatalf("recorded %d scenarios, want %d", len(findings.Scenarios), tc.wantScenarioCount)
			}
			if tc.wantDescription == "" {
				for _, item := range findings.Items {
					if strings.Contains(item.Description, "live validation verdict") {
						t.Fatalf("unexpected verdict finding on a passing run: %q", item.Description)
					}
				}
				return
			}
			var matched *types.Finding
			for i, item := range findings.Items {
				if strings.Contains(item.Description, tc.wantDescription) {
					matched = &findings.Items[i]
				}
			}
			if matched == nil {
				t.Fatalf("no finding carrying %q in %s", tc.wantDescription, outcome.Findings)
			}
			if matched.Severity == types.FindingSeverityInfo {
				t.Fatalf("verdict finding must block, got severity %q", matched.Severity)
			}
		})
	}
}

// TestTestStep_MissingScenarioContractFails proves the contract is required
// rather than advisory: an evidence turn that answers without scenarios or
// without a verdict has not answered, and a bad verdict is not silently
// accepted as "unknown".
func TestTestStep_MissingScenarioContractFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		output  string
		wantErr string
	}{
		{
			name:    "no scenarios array",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"verdict":"go"}`,
			wantErr: "missing scenarios array",
		},
		{
			name:    "empty scenarios array",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[],"verdict":"inconclusive"}`,
			wantErr: "empty scenarios array",
		},
		{
			name:    "no verdict",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"pass","live":true,"evidence":"ok","reason":""}]}`,
			wantErr: "missing verdict",
		},
		{
			name:    "verdict outside the vocabulary",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"pass","live":true,"evidence":"ok","reason":""}],"verdict":"probably fine"}`,
			wantErr: "is not one of",
		},
		{
			name:    "scenario result outside the vocabulary",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"maybe","live":true,"evidence":"ok","reason":""}],"verdict":"go"}`,
			wantErr: "is not one of",
		},
		{
			name:    "unnamed scenario",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"  ","result":"pass","live":true,"evidence":"ok","reason":""}],"verdict":"go"}`,
			wantErr: "missing name",
		},
		{
			name:    "missing declared scenario fields",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"pass"}],"verdict":"go"}`,
			wantErr: "missing live",
		},
		{
			name:    "pass must be live",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"pass","live":false,"evidence":"ok","reason":""}],"verdict":"go"}`,
			wantErr: "requires live validation",
		},
		{
			name:    "pass requires evidence",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"pass","live":true,"evidence":"  ","reason":""}],"verdict":"go"}`,
			wantErr: "missing evidence",
		},
		{
			name:    "fail requires evidence",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"fail","live":true,"evidence":"","reason":""}],"verdict":"no-go"}`,
			wantErr: "missing evidence",
		},
		{
			name:    "untested cannot be live",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"untested","live":true,"evidence":"","reason":"no browser"}],"verdict":"inconclusive"}`,
			wantErr: "untested but marked live",
		},
		{
			name:    "go cannot override failure",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"fail","live":true,"evidence":"failure","reason":""}],"verdict":"go"}`,
			wantErr: `verdict "go" contradicts failed scenario`,
		},
		{
			name:    "inconclusive cannot override failure",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"fail","live":true,"evidence":"failure","reason":""}],"verdict":"inconclusive"}`,
			wantErr: `verdict "inconclusive" contradicts failed scenario`,
		},
		{
			name:    "protocol vocabulary is exact",
			output:  `{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"x","result":"Pass","live":true,"evidence":"ok","reason":""}],"verdict":"go"}`,
			wantErr: "is not one of",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			output := tc.output
			ag := &mockAgent{
				name: "test",
				runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					return &agent.Result{Output: json.RawMessage(output)}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			_, err := (&TestStep{}).Execute(sctx)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Execute() error = %v, want one naming %q", err, tc.wantErr)
			}
		})
	}
}
