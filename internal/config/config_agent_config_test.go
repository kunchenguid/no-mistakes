package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func writeGlobalConfig(t *testing.T, data string) *GlobalConfig {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	return cfg
}

func loadGlobalConfigError(t *testing.T, data string) error {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatalf("LoadGlobal accepted %q, want an error", data)
	}
	return err
}

func TestLoadGlobal_AgentConfig(t *testing.T) {
	cfg := writeGlobalConfig(t, `
agent_config:
  codex:
    model: gpt-5.4
    effort: low
  claude:
    effort: high
  cursor:
    model: gpt-5
`)
	want := map[string]agentcfg.Profile{
		"codex":  {Model: "gpt-5.4", Effort: agentcfg.EffortLow},
		"claude": {Effort: agentcfg.EffortHigh},
		"cursor": {Model: "gpt-5"},
	}
	for name, wantProfile := range want {
		if got := cfg.AgentConfig[name]; got != wantProfile {
			t.Errorf("AgentConfig[%q] = %#v, want %#v", name, got, wantProfile)
		}
	}
}

func TestLoadGlobal_InvocationRoutesSelectAgentAndProfile(t *testing.T) {
	cfg := writeGlobalConfig(t, `
agent: codex
agent_config:
  pi:
    model: openrouter/base
    effort: medium
  codex:
    effort: low
invocations:
  review:
    agent: pi
    model: openrouter/reviewer
    effort: high
  review-fix:
    agent: codex
    model: gpt-5.6-sol
`)
	merged := Merge(cfg, &RepoConfig{})

	if got := merged.AgentProfileFor(types.AgentPi); got != (agentcfg.Profile{Model: "openrouter/base", Effort: agentcfg.EffortMedium}) {
		t.Fatalf("AgentProfileFor(pi) = %#v", got)
	}
	review, ok := merged.AgentInvocationFor("review")
	if !ok || review.Agent != types.AgentPi || review.Profile != (agentcfg.Profile{Model: "openrouter/reviewer", Effort: agentcfg.EffortHigh}) {
		t.Fatalf("review route = %#v, %v", review, ok)
	}
	fix, ok := merged.AgentInvocationFor("review-fix")
	if !ok || fix.Agent != types.AgentCodex || fix.Profile != (agentcfg.Profile{Model: "gpt-5.6-sol", Effort: agentcfg.EffortLow}) {
		t.Fatalf("review-fix route = %#v, %v; want inherited codex low effort", fix, ok)
	}
	if _, ok := merged.AgentInvocationFor("lint"); ok {
		t.Fatal("unconfigured invocation unexpectedly has a route")
	}
	if got := merged.AgentInvocationPurposes(); len(got) != 2 || got[0] != "review" || got[1] != "review-fix" {
		t.Fatalf("AgentInvocationPurposes() = %v", got)
	}
}

func TestLoadGlobal_AgentConfigRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"unknown agent", "agent_config:\n  gemini:\n    model: x\n", "invalid agent name in agent_config"},
		{"unknown effort", "agent_config:\n  codex:\n    effort: turbo\n", "invalid effort"},
		{"unknown knob", "agent_config:\n  codex:\n    temperature: 0.2\n", "temperature"},
		{"unknown invocation", "invocations:\n  document:\n    agent: codex\n", "purpose \"document\""},
		{"missing invocation agent", "invocations:\n  review:\n    model: x\n", "agent is required"},
		{"unknown invocation agent", "invocations:\n  review:\n    agent: gemini\n", "invalid invocations.review.agent"},
		{"unknown invocation knob", "invocations:\n  review:\n    agent: codex\n    temperature: 0.2\n", "temperature"},
		{"unknown invocation effort", "invocations:\n  review-fix:\n    agent: codex\n    effort: turbo\n", "invalid effort"},
		{"unmappable invocation effort", "invocations:\n  review:\n    agent: cursor\n    effort: high\n", "cannot express effort"},
		{"unmappable model", "agent_config:\n  rovodev:\n    model: x\n", "cannot express model"},
		{"unmappable effort", "agent_config:\n  antigravity:\n    effort: high\n", "cannot express effort"},
		{"acp effort", "agent_config:\n  cursor:\n    effort: high\n", "cannot express effort"},
		{"opencode bare model", "agent_config:\n  opencode:\n    model: gpt-5\n", "provider/model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := loadGlobalConfigError(t, tt.yaml)
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestLoadGlobal_AgentConfigAcceptsExplicitACPTarget(t *testing.T) {
	cfg := writeGlobalConfig(t, "agent_config:\n  acp:gemini:\n    model: gemini-3\n")
	if got := cfg.AgentConfig["acp:gemini"]; got.Model != "gemini-3" {
		t.Fatalf("AgentConfig[acp:gemini] = %#v", got)
	}
}

// TestLoadGlobal_AgentConfigAbsentIsZero is the backwards-compatibility floor:
// a config that predates agent_config resolves every agent to the zero Profile,
// which changes no argv anywhere.
func TestLoadGlobal_AgentConfigAbsentIsZero(t *testing.T) {
	cfg := writeGlobalConfig(t, "agent: codex\nagent_args_override:\n  codex:\n    - -m\n    - gpt-5.4\n")
	if cfg.AgentConfig != nil {
		t.Fatalf("AgentConfig = %#v, want nil for a config that does not set it", cfg.AgentConfig)
	}
	if cfg.Invocations != nil {
		t.Fatalf("Invocations = %#v, want nil for a config that does not set it", cfg.Invocations)
	}
	merged := Merge(cfg, &RepoConfig{})
	if got := merged.AgentProfileFor(types.AgentCodex); !got.IsZero() {
		t.Fatalf("AgentProfileFor(codex) = %#v, want the zero profile", got)
	}
	if got := merged.AgentArgsFor(types.AgentCodex); len(got) != 2 || got[0] != "-m" {
		t.Fatalf("AgentArgsFor(codex) = %v, want the untouched override", got)
	}
}

// TestAgentConfigAndArgsOverrideCoexistWithRawWinning states the precedence rule
// in the terms an operator reads it: a raw flag beats the common field for the
// same knob, and the common field still supplies every knob the raw args leave
// alone.
func TestAgentConfigAndArgsOverrideCoexistWithRawWinning(t *testing.T) {
	cfg := writeGlobalConfig(t, `
agent: codex
agent_config:
  codex:
    model: gpt-5.4
    effort: low
agent_args_override:
  codex:
    - -m
    - o3
`)
	merged := Merge(cfg, &RepoConfig{})
	raw := merged.AgentArgsFor(types.AgentCodex)
	profile := merged.AgentProfileFor(types.AgentCodex)
	got := agentcfg.NativeArgs(types.AgentCodex, profile, raw)
	want := []string{"-c", `model_reasoning_effort="low"`}
	if len(got) != len(want) {
		t.Fatalf("mapped args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mapped args = %v, want %v", got, want)
		}
	}
}

func TestInvocationRouteUsesSelectedAgentRawArgsWithRawWinning(t *testing.T) {
	cfg := writeGlobalConfig(t, `
agent: pi
invocations:
  review-fix:
    agent: codex
    model: gpt-5.6-sol
    effort: medium
agent_args_override:
  codex:
    - -m
    - operator-pin
`)
	merged := Merge(cfg, &RepoConfig{})
	route, ok := merged.AgentInvocationFor("review-fix")
	if !ok {
		t.Fatal("review-fix route is absent")
	}
	got := agentcfg.NativeArgs(route.Agent, route.Profile, merged.AgentArgsFor(route.Agent))
	want := []string{"-c", `model_reasoning_effort="medium"`}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("mapped route args = %v, want %v; raw model pin must win", got, want)
	}
}

func TestMerge_PreservesAgentConfig(t *testing.T) {
	global := DefaultGlobalConfig()
	global.AgentConfig = map[string]agentcfg.Profile{
		"claude": {Model: "sonnet", Effort: agentcfg.EffortHigh},
	}
	cfg := Merge(global, &RepoConfig{})
	if got := cfg.AgentProfileFor(types.AgentClaude); got.Model != "sonnet" || got.Effort != agentcfg.EffortHigh {
		t.Fatalf("AgentProfileFor(claude) = %#v", got)
	}
	if got := cfg.AgentProfileFor(types.AgentCodex); !got.IsZero() {
		t.Fatalf("AgentProfileFor(codex) = %#v, want zero", got)
	}
}

// TestAgentProfileFollowsTheSelectedAgent covers the accessor the wizard uses.
func TestAgentProfileFollowsTheSelectedAgent(t *testing.T) {
	cfg := &Config{
		Agent:       types.AgentPi,
		AgentConfig: map[string]agentcfg.Profile{"pi": {Effort: agentcfg.EffortMax}},
	}
	if got := cfg.AgentProfile(); got.Effort != agentcfg.EffortMax {
		t.Fatalf("AgentProfile() = %#v", got)
	}
}

// TestRepoConfigCannotSetAgentConfig keeps model and effort selection on the
// operator's machine: they decide which model runs with the maintainer's
// credentials, exactly like agent_args_override.
func TestRepoConfigCannotSetAgentConfig(t *testing.T) {
	repo, err := LoadRepoFromBytes([]byte("agent_config:\n  codex:\n    model: attacker-model\n"))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes: %v", err)
	}
	cfg := Merge(DefaultGlobalConfig(), repo)
	if got := cfg.AgentProfileFor(types.AgentCodex); !got.IsZero() {
		t.Fatalf("a repo config set an agent profile: %#v", got)
	}
}

func TestRepoConfigCannotSetInvocations(t *testing.T) {
	repo, err := LoadRepoFromBytes([]byte("invocations:\n  review:\n    agent: codex\n    model: attacker-model\n"))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes: %v", err)
	}
	cfg := Merge(DefaultGlobalConfig(), repo)
	if cfg.Invocations != nil {
		t.Fatalf("a repo config set invocation routes: %#v", cfg.Invocations)
	}
}

// TestDefaultConfigYAML_DocumentsAgentConfig keeps the shipped config commentary
// honest about the field that replaces knowing each harness's own flag.
func TestDefaultConfigYAML_DocumentsAgentConfig(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte(defaultConfigYAML))
	if err != nil {
		t.Fatalf("default config does not load: %v", err)
	}
	if cfg.AgentConfig != nil {
		t.Fatalf("default config activates agent_config: %#v", cfg.AgentConfig)
	}
	if !strings.Contains(defaultConfigYAML, "# agent_config:") {
		t.Fatal("default config.yaml does not document agent_config")
	}
	if !strings.Contains(defaultConfigYAML, "# invocations:") {
		t.Fatal("default config.yaml does not document review and review-fix routes")
	}
	for _, effort := range agentcfg.EffortNames() {
		if !strings.Contains(defaultConfigYAML, effort) {
			t.Errorf("default config.yaml does not list the %q effort level", effort)
		}
	}
}
