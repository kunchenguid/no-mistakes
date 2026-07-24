package agent

import (
	"reflect"
	"strings"
	"testing"
)

func TestResolveExtraArgs_FallsBackToConfiguredArgs(t *testing.T) {
	configured := []string{"-m", "gpt-5.4"}
	tests := []struct {
		name string
		opts RunOpts
	}{
		{"no per-step profile", RunOpts{}},
		{"profile names another agent", RunOpts{StepArgsOverride: map[string][]string{"claude": {"--model", "haiku"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.opts.resolveExtraArgs("codex", configured); !reflect.DeepEqual(got, configured) {
				t.Errorf("resolveExtraArgs = %v, want the configured args %v", got, configured)
			}
		})
	}
}

func TestResolveExtraArgs_ProfileReplacesConfiguredArgs(t *testing.T) {
	opts := RunOpts{StepArgsOverride: map[string][]string{"codex": {"-m", "gpt-5.4-mini"}}}
	if got := opts.resolveExtraArgs("codex", []string{"-m", "gpt-5.4"}); !reflect.DeepEqual(got, []string{"-m", "gpt-5.4-mini"}) {
		t.Errorf("resolveExtraArgs = %v, want the per-step profile to replace the global args", got)
	}
	// An explicit empty profile clears the global args for this step rather
	// than silently falling back to them.
	if got := opts.resolveExtraArgs("codex", nil); !reflect.DeepEqual(got, []string{"-m", "gpt-5.4-mini"}) {
		t.Errorf("resolveExtraArgs with no configured args = %v", got)
	}
}

func TestCodexBuildArgs_UsesPerStepProfile(t *testing.T) {
	a := &codexAgent{bin: "codex", extraArgs: []string{"-m", "gpt-5.4-mini"}}
	stepped := a.withStepArgs(RunOpts{StepArgsOverride: map[string][]string{
		"codex": {"-m", "gpt-5.4", "-c", `model_reasoning_effort="high"`},
	}})
	args := stepped.buildArgs("review the diff", "", "")
	if !argsContainPair(args, "-m", "gpt-5.4") {
		t.Errorf("buildArgs = %v, want the per-step model", args)
	}
	if !argsContainPair(args, "-c", `model_reasoning_effort="high"`) {
		t.Errorf("buildArgs = %v, want the per-step reasoning effort", args)
	}
	if strings.Contains(strings.Join(args, " "), "gpt-5.4-mini") {
		t.Errorf("buildArgs = %v, must not keep the global model when a per-step profile applies", args)
	}
	// The receiver is untouched, so the next step without a profile gets the
	// globally configured args back.
	if !argsContainPair(a.buildArgs("p", "", ""), "-m", "gpt-5.4-mini") {
		t.Error("withStepArgs must not mutate the shared adapter")
	}
}

func TestCodexBuildArgs_PerStepProfileKeepsManagedFlags(t *testing.T) {
	a := &codexAgent{bin: "codex", disableProjectSettings: true}
	args := a.withStepArgs(RunOpts{StepArgsOverride: map[string][]string{"codex": {"-m", "gpt-5.4"}}}).
		buildArgs("review", "", "")
	if !argsContainPair(args, "-c", "project_doc_max_bytes=0") {
		t.Errorf("buildArgs = %v, want project-settings suppression preserved under a per-step profile", args)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--dangerously-bypass-approvals-and-sandbox") || !strings.Contains(joined, "--json") {
		t.Errorf("buildArgs = %v, want the managed flags preserved", args)
	}
}

func TestClaudeBuildArgs_UsesPerStepProfile(t *testing.T) {
	a := &claudeAgent{bin: "claude", extraArgs: []string{"--model", "haiku"}}
	args := a.withStepArgs(RunOpts{StepArgsOverride: map[string][]string{"claude": {"--model", "opus"}}}).
		buildArgs(nil, "")
	if !argsContainPair(args, "--model", "opus") {
		t.Errorf("buildArgs = %v, want the per-step model", args)
	}
	if !strings.Contains(strings.Join(args, " "), "--dangerously-skip-permissions") {
		t.Errorf("buildArgs = %v, want the managed permission flag preserved", args)
	}
}

func TestClaudeWithStepArgs_IgnoresAnotherAgentsProfile(t *testing.T) {
	a := &claudeAgent{bin: "claude", extraArgs: []string{"--model", "haiku"}}
	stepped := a.withStepArgs(RunOpts{StepArgsOverride: map[string][]string{"codex": {"-m", "gpt-5.4"}}})
	if stepped != a {
		t.Fatal("an unrelated agent's profile must not produce a modified adapter")
	}
	if !argsContainPair(stepped.buildArgs(nil, ""), "--model", "haiku") {
		t.Error("claude must keep its global args when the profile names only codex")
	}
}

func TestCopilotBuildArgs_UsesPerStepProfile(t *testing.T) {
	a := &copilotAgent{bin: "copilot", extraArgs: []string{"--model", "cheap"}}
	args := a.withStepArgs(RunOpts{StepArgsOverride: map[string][]string{"copilot": {"--model", "strong", "--effort", "high"}}}).
		buildArgs("p")
	if !argsContainPair(args, "--model", "strong") || !argsContainPair(args, "--effort", "high") {
		t.Errorf("buildArgs = %v, want the per-step profile", args)
	}
}

func TestPiBuildArgs_UsesPerStepProfile(t *testing.T) {
	a := &piAgent{bin: "pi", extraArgs: []string{"--model", "cheap"}}
	args := a.withStepArgs(RunOpts{StepArgsOverride: map[string][]string{"pi": {"--model", "strong"}}}).buildArgs()
	if !argsContainPair(args, "--model", "strong") {
		t.Errorf("buildArgs = %v, want the per-step profile", args)
	}
	if !argsContainPair(args, "--mode", "json") {
		t.Errorf("buildArgs = %v, want the managed flags preserved", args)
	}
}
