package steps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type recordingPublicationCommandRunner struct {
	calls   []pipeline.PublicationCommandRequest
	results []pipeline.PublicationCommandResult
}

func (r *recordingPublicationCommandRunner) RunPublicationCommand(_ context.Context, request pipeline.PublicationCommandRequest) (pipeline.PublicationCommandResult, error) {
	r.calls = append(r.calls, request)
	if len(r.results) == 0 {
		return pipeline.PublicationCommandResult{}, errors.New("unexpected publication command")
	}
	result := r.results[0]
	r.results = r.results[1:]
	return result, nil
}

func TestPublicationDefenseAgentStepsUseInspectAndReportOnlyPrompts(t *testing.T) {
	tests := map[string]struct {
		step     pipeline.Step
		commands config.Commands
		output   json.RawMessage
	}{
		"review": {
			step:   &ReviewStep{},
			output: json.RawMessage(`{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`),
		},
		"test": {
			step:   &TestStep{},
			output: json.RawMessage(`{"findings":[],"summary":"clean","tested":["inspection"],"testing_summary":"clean","artifacts":[]}`),
		},
		"document": {
			step:     &DocumentStep{},
			commands: config.Commands{Lint: "echo configured-lint"},
			output:   json.RawMessage(`{"findings":[],"summary":"docs current"}`),
		},
		"lint": {
			step:   &LintStep{},
			output: json.RawMessage(`{"findings":[],"summary":"lint clean"}`),
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			gitCmd(t, dir, "checkout", "--detach", headSHA)
			ag := &mockAgent{
				name: "publication-defense",
				runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
					return &agent.Result{Output: test.output}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, test.commands)
			sctx.PublicationDefense = true
			sctx.Shared = &pipeline.RunShared{}

			outcome, err := test.step.Execute(sctx)
			if err != nil {
				t.Fatalf("execute publication-defense %s: %v", name, err)
			}
			if len(ag.calls) != 1 {
				t.Fatalf("%s agent calls=%d, want one audit pass", name, len(ag.calls))
			}
			assertPublicationAuditOnlyPrompt(t, ag.calls[0].Prompt)
			if outcome == nil || outcome.AutoFixable {
				t.Fatalf("%s defense outcome=%#v, must not request a fix", name, outcome)
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
				t.Fatalf("%s advanced HEAD to %s, want %s", name, got, headSHA)
			}
			if got := gitStatusPorcelain(t, dir); got != "" {
				t.Fatalf("%s changed candidate worktree: %s", name, got)
			}
		})
	}
}

func TestPublicationDefenseRejectsFixExecutionBeforeCallingAnAgent(t *testing.T) {
	for _, step := range []pipeline.Step{&ReviewStep{}, &TestStep{}, &DocumentStep{}, &LintStep{}} {
		t.Run(string(step.Name()), func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{
				name: "must-not-run",
				runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
					t.Fatal("publication defense reached a fix agent")
					return nil, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.PublicationDefense = true
			sctx.Fixing = true
			sctx.PreviousFindings = `{"findings":[{"severity":"warning","description":"fix me","action":"auto-fix"}]}`

			if _, err := step.Execute(sctx); err == nil || !strings.Contains(err.Error(), "publication defense") {
				t.Fatalf("%s fixing-state error=%v, want fail-closed publication defense error", step.Name(), err)
			}
			if len(ag.calls) != 0 {
				t.Fatalf("%s called agent %d times in forbidden fix state", step.Name(), len(ag.calls))
			}
		})
	}
}

func TestPublicationDefenseRunsConfiguredTestAndLintCommandsWithoutFixAgent(t *testing.T) {
	for _, test := range []struct {
		step     pipeline.Step
		commands config.Commands
	}{
		{step: &TestStep{}, commands: config.Commands{Test: "echo configured-test"}},
		{step: &LintStep{}, commands: config.Commands{Lint: "echo configured-lint"}},
	} {
		t.Run(string(test.step.Name()), func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{name: "must-not-run"}
			runner := &recordingPublicationCommandRunner{results: []pipeline.PublicationCommandResult{{Output: "configured pass\n", ExitCode: 0}}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, test.commands)
			sctx.PublicationDefense = true
			sctx.PublicationCommandRunner = runner

			outcome, err := test.step.Execute(sctx)
			if err != nil {
				t.Fatalf("run configured %s command: %v", test.step.Name(), err)
			}
			if outcome == nil || outcome.ExitCode != 0 || outcome.AutoFixable {
				t.Fatalf("configured %s outcome=%#v", test.step.Name(), outcome)
			}
			if len(ag.calls) != 0 {
				t.Fatalf("configured %s called a fix/evidence agent %d times", test.step.Name(), len(ag.calls))
			}
			if len(runner.calls) != 1 {
				t.Fatalf("configured %s boundary calls=%d, want one", test.step.Name(), len(runner.calls))
			}
		})
	}
}

func assertPublicationAuditOnlyPrompt(t *testing.T, prompt string) {
	t.Helper()
	for _, required := range []string{
		"Publication defense mode is read-only",
		"Inspect and report only",
		"Do not create, edit, delete, stage, commit, or fix files",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("publication defense prompt missing %q:\n%s", required, prompt)
		}
	}
}

func TestPublicationDefenseModeDefaultsOffForOrdinaryAXIContext(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{})
	if sctx.PublicationDefense {
		t.Fatal("ordinary AXI context unexpectedly enabled publication defense")
	}
	if sctx.Run.Kind == types.RunKindFactoryPublicationV1 {
		t.Fatal("ordinary test context unexpectedly uses publication run kind")
	}
}

func TestPublicationDefenseConfiguredTestAndLintUseBoundaryInsteadOfDirectShell(t *testing.T) {
	for _, test := range []struct {
		step     pipeline.Step
		commands config.Commands
		command  string
	}{
		{step: &TestStep{}, commands: config.Commands{Test: "exit 91"}, command: "exit 91"},
		{step: &LintStep{}, commands: config.Commands{Lint: "exit 92"}, command: "exit 92"},
	} {
		t.Run(string(test.step.Name()), func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			runner := &recordingPublicationCommandRunner{results: []pipeline.PublicationCommandResult{{Output: "confined pass\n", ExitCode: 0}}}
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "must-not-run"}, dir, baseSHA, headSHA, test.commands)
			sctx.PublicationDefense = true
			sctx.PublicationCommandRunner = runner

			outcome, err := test.step.Execute(sctx)
			if err != nil {
				t.Fatalf("execute configured %s through publication boundary: %v", test.step.Name(), err)
			}
			if outcome == nil || outcome.ExitCode != 0 || len(runner.calls) != 1 {
				t.Fatalf("configured %s outcome=%#v boundary calls=%d", test.step.Name(), outcome, len(runner.calls))
			}
			call := runner.calls[0]
			if call.Command != test.command || call.WorkDir != dir {
				t.Fatalf("configured %s boundary request=%#v", test.step.Name(), call)
			}
		})
	}
}

func TestPublicationDefenseConfiguredCommandWithoutBoundaryFailsBeforeDirectShell(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "must-not-run"}, dir, baseSHA, headSHA, config.Commands{Test: "printf unsafe"})
	sctx.PublicationDefense = true
	if _, err := (&TestStep{}).Execute(sctx); !errors.Is(err, agent.ErrPublicationConfinementUnavailable) {
		t.Fatalf("configured publication command without boundary error=%v, want confinement_unavailable", err)
	}
}
