package config

import (
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestApplyResolvedAgentRouteChangesOnlyMatchingAgent(t *testing.T) {
	cfg := &Config{
		Agent:  types.AgentCodex,
		Agents: []types.AgentName{types.AgentCodex, types.AgentClaude},
		AgentArgsOverride: map[string][]string{
			"pi": {"--existing"},
		},
	}
	route := &types.AgentRouteOverride{
		From: types.AgentClaude,
		To:   types.AgentPi,
		Args: []string{"--model", "kimi-coding/k3", "--thinking", "high"},
	}

	matched, err := cfg.ApplyResolvedAgentRoute(route)
	if err != nil {
		t.Fatalf("ApplyResolvedAgentRoute: %v", err)
	}
	if !matched {
		t.Fatal("Claude route did not match")
	}
	if want := []types.AgentName{types.AgentCodex, types.AgentPi}; !reflect.DeepEqual(cfg.Agents, want) {
		t.Fatalf("Agents = %v, want %v", cfg.Agents, want)
	}
	if cfg.Agent != types.AgentCodex {
		t.Fatalf("primary Agent = %q, want Codex unchanged", cfg.Agent)
	}
	wantArgs := []string{"--existing", "--model", "kimi-coding/k3", "--thinking", "high"}
	if got := cfg.AgentArgsFor(types.AgentPi); !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("Pi args = %v, want %v", got, wantArgs)
	}
}

func TestApplyResolvedAgentRouteDoesNothingWithoutSource(t *testing.T) {
	cfg := &Config{Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex}}
	route := &types.AgentRouteOverride{
		From: types.AgentClaude,
		To:   types.AgentPi,
		Args: []string{"--model", "kimi-coding/k3"},
	}

	matched, err := cfg.ApplyResolvedAgentRoute(route)
	if err != nil {
		t.Fatalf("ApplyResolvedAgentRoute: %v", err)
	}
	if matched {
		t.Fatal("Claude route matched a Codex-only configuration")
	}
	if cfg.Agent != types.AgentCodex || len(cfg.Agents) != 1 || cfg.Agents[0] != types.AgentCodex {
		t.Fatalf("Codex-only configuration changed: Agent=%q Agents=%v", cfg.Agent, cfg.Agents)
	}
	if got := cfg.AgentArgsFor(types.AgentPi); got != nil {
		t.Fatalf("Pi args were installed without a matching route: %v", got)
	}
}

func TestApplyResolvedAgentRouteRejectsManagedArgs(t *testing.T) {
	cfg := &Config{Agent: types.AgentClaude, Agents: []types.AgentName{types.AgentClaude}}
	route := &types.AgentRouteOverride{
		From: types.AgentClaude,
		To:   types.AgentPi,
		Args: []string{"--mode", "json"},
	}
	if _, err := cfg.ApplyResolvedAgentRoute(route); err == nil {
		t.Fatal("expected managed Pi flag to be rejected")
	}
}
