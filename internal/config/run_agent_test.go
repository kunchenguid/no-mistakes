package config

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestApplyRunAgentSelectsCodexForEveryRole(t *testing.T) {
	cfg := &Config{
		Agent:  types.AgentClaude,
		Agents: []types.AgentName{types.AgentClaude, types.AgentCodex},
		ReviewAgents: map[string]ReviewAgent{
			RoleReviewer: {Agent: types.AgentClaude},
			RoleFixer:    {Agent: types.AgentClaude},
		},
	}
	if err := cfg.ApplyRunAgent(agentcfg.RunAgentCodex); err != nil {
		t.Fatal(err)
	}
	if cfg.Agent != types.AgentCodex || len(cfg.Agents) != 1 || cfg.Agents[0] != types.AgentCodex || len(cfg.ReviewAgents) != 0 {
		t.Fatalf("run agent did not replace every role: %+v", cfg)
	}
}

func TestApplyRunAgentRejectsUnsupportedSelection(t *testing.T) {
	cfg := &Config{Agent: types.AgentClaude}
	if err := cfg.ApplyRunAgent(agentcfg.RunAgent("claude")); err == nil {
		t.Fatal("unsupported selection accepted")
	}
	if cfg.Agent != types.AgentClaude {
		t.Fatal("rejected selection changed config")
	}
}
