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

// TestNewPipelineAgent_ReviewAndFixUseDifferentHarnesses exercises the
// production construction and routing path with fake Pi and Codex processes.
// It proves one pipeline can review with Pi, repair with Codex, and retain the
// default harness for every unconfigured purpose.
func TestNewPipelineAgent_ReviewAndFixUseDifferentHarnesses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake is Unix-specific")
	}
	dir := t.TempDir()
	piArgsPath := filepath.Join(dir, "pi-args.log")
	codexArgsPath := filepath.Join(dir, "codex-args.log")
	piPath := filepath.Join(dir, "pi")
	codexPath := filepath.Join(dir, "codex")
	piScript := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' \"$*\" >> \"" + piArgsPath + "\"\nprintf '%s\\n' '{\"type\":\"agent_end\",\"messages\":[{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}]}'\n"
	codexScript := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' \"$*\" >> \"" + codexArgsPath + "\"\nprintf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"ok\"}}'\nprintf '%s\\n' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	if err := os.WriteFile(piPath, []byte(piScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, []byte(codexScript), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Agent:             types.AgentPi,
		AgentPathOverride: map[string]string{"pi": piPath, "codex": codexPath},
		AgentConfig: map[string]agentcfg.Profile{
			"pi": {Model: "openrouter/base", Effort: agentcfg.EffortMedium},
		},
		Invocations: map[string]config.AgentInvocation{
			"review":     {Agent: types.AgentPi, Profile: agentcfg.Profile{Model: "openrouter/z-ai/glm-5.3-flash", Effort: agentcfg.EffortHigh}},
			"review-fix": {Agent: types.AgentCodex, Profile: agentcfg.Profile{Model: "gpt-5.6-sol", Effort: agentcfg.EffortMedium}},
		},
	}
	ag, err := newPipelineAgent(context.Background(), cfg, dir, func(bin string) (string, error) { return bin, nil }, runenv.Overlay{})
	if err != nil {
		t.Fatalf("newPipelineAgent: %v", err)
	}
	defer ag.Close()

	wantProviders := map[string]string{"review": "pi", "review-fix": "codex", "lint": "pi"}
	for _, purpose := range []string{"review", "review-fix", "lint"} {
		result, err := ag.Run(context.Background(), agent.RunOpts{Prompt: "work", Purpose: purpose, CWD: dir})
		if err != nil {
			t.Fatalf("Run(%s): %v", purpose, err)
		}
		if result.Provider != wantProviders[purpose] {
			t.Fatalf("Run(%s) provider = %q, want %q", purpose, result.Provider, wantProviders[purpose])
		}
	}
	piRaw, err := os.ReadFile(piArgsPath)
	if err != nil {
		t.Fatal(err)
	}
	piLines := strings.Split(strings.TrimSpace(string(piRaw)), "\n")
	if len(piLines) != 2 {
		t.Fatalf("Pi argv lines = %q, want review and lint", piLines)
	}
	for i, want := range []string{
		"--model openrouter/z-ai/glm-5.3-flash --thinking high",
		"--model openrouter/base --thinking medium",
	} {
		if !strings.Contains(piLines[i], want) {
			t.Errorf("Pi argv[%d] = %q, want it to contain %q", i, piLines[i], want)
		}
	}
	codexRaw, err := os.ReadFile(codexArgsPath)
	if err != nil {
		t.Fatal(err)
	}
	codexArgs := strings.TrimSpace(string(codexRaw))
	for _, want := range []string{"-m gpt-5.6-sol", `-c model_reasoning_effort="medium"`} {
		if !strings.Contains(codexArgs, want) {
			t.Errorf("Codex argv = %q, want it to contain %q", codexArgs, want)
		}
	}
}

func TestNewPipelineAgent_InvocationAgentMustBeRunnable(t *testing.T) {
	cfg := &config.Config{
		Agent: types.AgentCodex,
		Invocations: map[string]config.AgentInvocation{
			"review": {Agent: types.AgentPi},
		},
	}
	_, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), func(bin string) (string, error) {
		if bin == "codex" {
			return "/fake/bin/codex", nil
		}
		return "", os.ErrNotExist
	}, runenv.Overlay{})
	if err == nil || !strings.Contains(err.Error(), `invocations.review agent "pi" is not runnable`) {
		t.Fatalf("newPipelineAgent error = %v, want routed-agent setup refusal", err)
	}
}
