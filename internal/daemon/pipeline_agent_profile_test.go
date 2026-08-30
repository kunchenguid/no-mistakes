package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestNewPipelineAgent_ThreadsTheAgentProfile proves the pipeline half of the
// unified tuning layer: agent_config reaches agent construction per agent, so a
// knob the harness cannot express fails the run at setup instead of being
// silently dropped into a review nobody can reproduce.
func TestNewPipelineAgent_ThreadsTheAgentProfile(t *testing.T) {
	cfg := &config.Config{
		Agent:       types.AgentAntigravity,
		AgentConfig: map[string]agentcfg.Profile{"antigravity": {Model: "some-model"}},
	}
	_, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err == nil {
		t.Fatal("a model on a harness that cannot express one must fail setup")
	}
	if !strings.Contains(err.Error(), "cannot express model") {
		t.Fatalf("refusal should explain the reason, got: %v", err)
	}
}

func TestNewPipelineAgent_ProfileIsPerAgent(t *testing.T) {
	cfg := &config.Config{
		Agents:      []types.AgentName{types.AgentClaude, types.AgentAntigravity},
		AgentConfig: map[string]agentcfg.Profile{"claude": {Model: "sonnet", Effort: agentcfg.EffortHigh}},
	}
	ag, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err != nil {
		t.Fatalf("a profile set only for claude must not reach antigravity: %v", err)
	}
	_ = ag.Close()
}

// TestNewPipelineAgent_NoProfileIsUnchanged is the backwards-compatibility
// floor at the daemon: a configuration that predates agent_config builds every
// agent exactly as before.
func TestNewPipelineAgent_NoProfileIsUnchanged(t *testing.T) {
	cfg := &config.Config{
		Agent:             types.AgentCodex,
		AgentArgsOverride: map[string][]string{"codex": {"-m", "gpt-5.4"}},
	}
	ag, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err != nil {
		t.Fatalf("newPipelineAgent = %v", err)
	}
	_ = ag.Close()
}

// TestNewPipelineAgent_ReviewAndFixUseDifferentModels exercises the production
// construction and routing path with a fake Pi process. It proves two
// invocations in one pipeline agent receive different native model flags while
// an unconfigured purpose keeps the harness-wide fallback.
func TestNewPipelineAgent_ReviewAndFixUseDifferentModels(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake is Unix-specific")
	}
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.log")
	piPath := filepath.Join(dir, "pi")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' \"$*\" >> \"" + argsPath + "\"\nprintf '%s\\n' '{\"type\":\"agent_end\",\"messages\":[{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}]}'\n"
	if err := os.WriteFile(piPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Agent:             types.AgentPi,
		AgentPathOverride: map[string]string{"pi": piPath},
		AgentConfig: map[string]agentcfg.Profile{
			"pi": {Model: "openrouter/base", Effort: agentcfg.EffortMedium},
		},
		AgentInvocationConfig: map[string]map[string]agentcfg.Profile{
			"pi": {
				"review":     {Model: "openrouter/reviewer", Effort: agentcfg.EffortHigh},
				"review-fix": {Model: "openrouter/fixer"},
			},
		},
	}
	ag, err := newPipelineAgent(context.Background(), cfg, dir, func(bin string) (string, error) { return bin, nil }, runenv.Overlay{})
	if err != nil {
		t.Fatalf("newPipelineAgent: %v", err)
	}
	defer ag.Close()

	for _, purpose := range []string{"review", "review-fix", "lint"} {
		if _, err := ag.Run(context.Background(), agent.RunOpts{Prompt: "work", Purpose: purpose, CWD: dir}); err != nil {
			t.Fatalf("Run(%s): %v", purpose, err)
		}
	}
	raw, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("Pi argv lines = %q, want three invocations", lines)
	}
	for i, want := range []string{
		"--model openrouter/reviewer --thinking high",
		"--model openrouter/fixer --thinking medium",
		"--model openrouter/base --thinking medium",
	} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("argv[%d] = %q, want it to contain %q", i, lines[i], want)
		}
	}
}
