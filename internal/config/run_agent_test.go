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
	afterRound := 3
	global.ReviewAgents = map[string]ReviewAgent{
		RoleReviewer:           {Agent: types.AgentCodex, Model: "strong-reviewer", Effort: agentcfg.EffortHigh},
		RoleFixer:              {Agent: types.AgentCodex, Model: "strong-fixer", Effort: agentcfg.EffortHigh},
		RoleReviewerAfterRound: {Agent: types.AgentCodex, Model: "late-reviewer", Effort: agentcfg.EffortHigh, AfterRound: &afterRound},
		RoleFixerAfterRound:    {Agent: types.AgentCodex, Model: "late-fixer", Effort: agentcfg.EffortHigh},
	}
	global.AgentConfig = map[string]agentcfg.Profile{"codex": {Model: "configured-model", Effort: agentcfg.EffortLow}}
	first, second := Merge(global, &RepoConfig{}), Merge(global, &RepoConfig{})
	first.DisableProjectSettings = true
	if err := first.ApplyRunAgent(types.AgentCodex, nil); err != nil {
		t.Fatal(err)
	}
	if first.Agent != types.AgentCodex || !reflect.DeepEqual(first.Agents, []types.AgentName{types.AgentCodex}) || first.QuotaRouting.Enabled {
		t.Fatalf("routing can still switch: %+v", first)
	}
	if !reflect.DeepEqual(first.ReviewAgents, global.ReviewAgents) {
		t.Fatalf("same-engine profiles were lost: %+v", first.ReviewAgents)
	}
	for role, entry := range first.ReviewAgents {
		cfg := first.ForReviewAgent(entry)
		if cfg.Agent != types.AgentCodex || cfg.AgentProfile() != (agentcfg.Profile{Model: entry.Model, Effort: agentcfg.EffortHigh}) || !cfg.DisableProjectSettings {
			t.Fatalf("%s lost its profile or instruction suppression: %+v", role, cfg)
		}
	}
	if first.ReviewAgentTakeoverRound(RoleReviewerAfterRound) != 4 || first.ReviewAgentTakeoverRound(RoleFixerAfterRound) != 2 {
		t.Fatal("selection changed overlay takeover rounds")
	}
	*first.ReviewAgents[RoleReviewerAfterRound].AfterRound = 7
	delete(first.ReviewAgents, RoleFixer)
	first.ReviewAgents[RoleReviewer] = ReviewAgent{Agent: types.AgentCodex, Model: "run-only"}
	if second.Agent != types.AgentClaude || len(second.Agents) != 2 || !reflect.DeepEqual(second.ReviewAgents, global.ReviewAgents) || global.Agent != types.AgentClaude || global.ReviewAgents[RoleReviewer].Model != "strong-reviewer" || len(global.ReviewAgents) != 4 || afterRound != 3 {
		t.Fatal("selection leaked to another run or global configuration")
	}
	if !reflect.DeepEqual(first.AgentConfig, global.AgentConfig) || !first.DisableProjectSettings {
		t.Fatal("backend selection changed model settings or instruction suppression")
	}
	if err := first.ApplyRunAgent(types.AgentClaude, &agentcfg.PiProfile{Model: "provider/model", Effort: agentcfg.EffortHigh}); err == nil {
		t.Fatal("conflicting Pi pin was accepted")
	}
}

func TestRunAgentSuppressesOnlyOtherEngineRoleProfiles(t *testing.T) {
	for _, selected := range []types.AgentName{types.AgentCodex, types.AgentClaude} {
		for _, matchingRole := range ReviewAgentRoles {
			t.Run(string(selected)+"/"+matchingRole, func(t *testing.T) {
				other := types.AgentClaude
				if selected == other {
					other = types.AgentCodex
				}
				global := DefaultGlobalConfig()
				global.ReviewAgents = make(map[string]ReviewAgent)
				for _, role := range ReviewAgentRoles {
					name := other
					if role == matchingRole {
						name = selected
					}
					global.ReviewAgents[role] = ReviewAgent{Agent: name, Model: role, Effort: agentcfg.EffortHigh}
				}
				cfg := Merge(global, &RepoConfig{})
				if err := cfg.ApplyRunAgent(selected, nil); err != nil {
					t.Fatal(err)
				}
				if len(cfg.ReviewAgents) != 1 || cfg.ReviewAgents[matchingRole] != global.ReviewAgents[matchingRole] {
					t.Fatalf("wrong role profiles retained: %+v", cfg.ReviewAgents)
				}
				if len(global.ReviewAgents) != len(ReviewAgentRoles) {
					t.Fatal("filtering mutated global profiles")
				}
			})
		}
	}
}
