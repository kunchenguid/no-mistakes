package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func stepNames(steps []pipeline.Step) []string {
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, string(step.Name()))
	}
	return names
}

// The core sequence must survive gate insertion intact: same members, same
// relative order. A gate can lengthen a run, never reshape it.
func TestWithCustomGates_InsertsAfterAnchorAndPreservesCore(t *testing.T) {
	got := stepNames(WithCustomGates(AllSteps(), []config.Gate{
		{Name: "mutation-budget", After: types.StepTest, Command: "make mutation"},
		{Name: "arch-fitness", After: types.StepReview, Instructions: "no cycles"},
	}))
	want := []string{
		"intent", "rebase", "review", "gate:review:arch-fitness",
		"test", "gate:test:mutation-budget", "document", "lint", "push", "pr", "ci",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("sequence =\n %v\nwant\n %v", got, want)
	}
}

func TestWithCustomGates_KeepsDeclarationOrderOnSharedAnchor(t *testing.T) {
	got := stepNames(WithCustomGates(AllSteps(), []config.Gate{
		{Name: "first", After: types.StepLint, Command: "a"},
		{Name: "second", After: types.StepLint, Command: "b"},
	}))
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "lint,gate:lint:first,gate:lint:second,push") {
		t.Fatalf("sequence = %v, want the two lint gates in declaration order", got)
	}
}

func TestWithCustomGates_NoGatesReturnsCore(t *testing.T) {
	if got, want := len(WithCustomGates(AllSteps(), nil)), len(AllSteps()); got != want {
		t.Fatalf("step count = %d, want the core %d", got, want)
	}
}

func TestCustomGateStep_CommandPassRunsClean(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{name: "mock"}, dir, baseSHA, headSHA, config.Commands{})

	step := &CustomGateStep{Gate: config.Gate{Name: "always-pass", After: types.StepTest, Command: "exit 0"}}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if outcome.NeedsApproval || outcome.ExitCode != 0 || outcome.Findings != "" {
		t.Fatalf("outcome = %+v, want a clean pass", outcome)
	}
}

// A failing gate parks for a human rather than auto-fixing: the pipeline cannot
// know what fixing an arbitrary repository check means.
func TestCustomGateStep_CommandFailureParksForHuman(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{name: "mock"}, dir, baseSHA, headSHA, config.Commands{})

	step := &CustomGateStep{Gate: config.Gate{Name: "always-fail", After: types.StepTest, Command: "exit 3"}}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if !outcome.NeedsApproval {
		t.Error("NeedsApproval = false, want the gate to park")
	}
	if outcome.AutoFixable {
		t.Error("AutoFixable = true, want a gate failure to stay the author's call")
	}
	if outcome.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", outcome.ExitCode)
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("findings did not parse: %v", err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("findings = %+v, want one ask-user finding", findings.Items)
	}
}

func TestCustomGateStep_AgentGateReportsFindingsAsAskUser(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "mock", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"error","description":"imports internal/cli","action":"auto-fix"}],"summary":"layering violation"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &CustomGateStep{Gate: config.Gate{Name: "arch-fitness", After: types.StepLint, Instructions: "No package under internal/ may import internal/cli."}}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %+v, want a parked, non-auto-fixable gate", outcome)
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("findings did not parse: %v", err)
	}
	// The gate states a repository rule; only a human can accept breaking it,
	// so the agent's own action classification is overridden.
	if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("findings = %+v, want the agent's action forced to ask-user", findings.Items)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, "No package under internal/ may import internal/cli.") {
		t.Error("gate instructions were not delivered to the agent")
	}
}

func TestCustomGateStep_AgentGateCleanRunPasses(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "mock", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"clean"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &CustomGateStep{Gate: config.Gate{Name: "arch-fitness", After: types.StepLint, Instructions: "no cycles"}}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if outcome.NeedsApproval || outcome.Findings != "" {
		t.Fatalf("outcome = %+v, want a clean pass", outcome)
	}
}
