package config

import (
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunAgentChangesOnlyTheRunsRouting(t *testing.T) {
	global := DefaultGlobalConfig()
	global.Agent, global.Agents = types.AgentClaude, []types.AgentName{types.AgentClaude, types.AgentCodex}
	global.ReviewAgents = map[string]ReviewAgent{RoleReviewer: {Agent: types.AgentClaude}}
	global.AgentConfig = map[string]agentcfg.Profile{"codex": {Model: "configured-model", Effort: agentcfg.EffortHigh}}
	first, second := Merge(global, &RepoConfig{}), Merge(global, &RepoConfig{})
	if err := first.ApplyRunAgent(types.AgentCodex, nil); err != nil {
		t.Fatal(err)
	}
	if first.Agent != types.AgentCodex || len(first.Agents) != 1 || len(first.ReviewAgents) != 0 || first.QuotaRouting.Enabled {
		t.Fatalf("routing can still switch: %+v", first)
	}
	if second.Agent != types.AgentClaude || len(second.Agents) != 2 || len(second.ReviewAgents) != 1 || global.Agent != types.AgentClaude {
		t.Fatal("selection leaked to another run or global configuration")
	}
	if !reflect.DeepEqual(first.AgentConfig, global.AgentConfig) || first.DisableProjectSettings != second.DisableProjectSettings {
		t.Fatal("backend selection changed model settings or instruction suppression")
	}
	if err := first.ApplyRunAgent(types.AgentClaude, &agentcfg.PiProfile{Model: "provider/model", Effort: agentcfg.EffortHigh}); err == nil {
		t.Fatal("conflicting Pi pin was accepted")
	}
}
