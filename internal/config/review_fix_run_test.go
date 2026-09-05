package config

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewFixRunConfigPinsSelectionAndProfile(t *testing.T) {
	started := &Config{
		Agent:  types.AgentPi,
		Agents: []types.AgentName{types.AgentPi},
		ReviewFixAgentConfig: map[string]ReviewFixProfile{
			"pi": {
				Profile: agentcfg.Profile{Model: "openai-codex/gpt-5.6-sol", Effort: agentcfg.EffortLow},
				Fast:    true,
			},
		},
	}
	encoded, err := started.MarshalReviewFixRunConfig()
	if err != nil {
		t.Fatal(err)
	}

	recovered := &Config{
		ReviewFixAgent:  types.AgentCodex,
		ReviewFixAgents: []types.AgentName{types.AgentCodex},
		ReviewFixAgentConfig: map[string]ReviewFixProfile{
			"codex": {Profile: agentcfg.Profile{Model: "changed"}},
		},
	}
	if err := recovered.ApplyReviewFixRunConfig(encoded); err != nil {
		t.Fatal(err)
	}
	if recovered.ReviewFixAgent != types.AgentPi || len(recovered.ReviewFixAgents) != 1 {
		t.Fatalf("restored fixer = %q %v", recovered.ReviewFixAgent, recovered.ReviewFixAgents)
	}
	got, ok := recovered.ReviewFixAgentConfig["pi"]
	want := ReviewFixProfile{
		Profile: agentcfg.Profile{Model: "openai-codex/gpt-5.6-sol", Effort: agentcfg.EffortLow},
		Fast:    true,
	}
	if !ok || got != want {
		t.Fatalf("restored profile = %#v, want %#v", got, want)
	}
}

func TestReviewFixRunConfigPinsEffectiveBaseProfileForSelectedFixer(t *testing.T) {
	started := &Config{
		Agent:           types.AgentClaude,
		Agents:          []types.AgentName{types.AgentClaude},
		ReviewFixAgent:  types.AgentPi,
		ReviewFixAgents: []types.AgentName{types.AgentPi},
		AgentConfig: map[string]agentcfg.Profile{
			"pi": {Model: "openai-codex/gpt-5.6-sol", Effort: agentcfg.EffortLow},
		},
	}
	encoded, err := started.MarshalReviewFixRunConfig()
	if err != nil {
		t.Fatal(err)
	}
	recovered := &Config{
		AgentConfig: map[string]agentcfg.Profile{"pi": {Model: "changed", Effort: agentcfg.EffortHigh}},
	}
	if err := recovered.ApplyReviewFixRunConfig(encoded); err != nil {
		t.Fatal(err)
	}
	got, explicit := recovered.ReviewFixProfileFor(types.AgentPi)
	want := agentcfg.Profile{Model: "openai-codex/gpt-5.6-sol", Effort: agentcfg.EffortLow}
	if !explicit || got.Profile != want {
		t.Fatalf("restored effective profile = %#v, explicit=%v, want %#v", got, explicit, want)
	}
}

func TestReviewFixRunConfigPinsAbsenceOfOverride(t *testing.T) {
	started := &Config{Agent: types.AgentClaude, Agents: []types.AgentName{types.AgentClaude}}
	encoded, err := started.MarshalReviewFixRunConfig()
	if err != nil {
		t.Fatal(err)
	}

	recovered := &Config{
		ReviewFixAgent:       types.AgentPi,
		ReviewFixAgents:      []types.AgentName{types.AgentPi},
		ReviewFixAgentConfig: map[string]ReviewFixProfile{"pi": {}},
	}
	if err := recovered.ApplyReviewFixRunConfig(encoded); err != nil {
		t.Fatal(err)
	}
	if recovered.HasReviewFixAgentOverride() || recovered.ReviewFixAgentConfig != nil {
		t.Fatalf("later global override leaked into existing run: %#v", recovered)
	}
}

func TestApplyReviewFixRunConfigFailsClosedOnInvalidStoredShape(t *testing.T) {
	cfg := &Config{}
	for _, encoded := range []string{
		`{"version":2,"enabled":false}`,
		`{"version":1,"enabled":true}`,
		`{"version":1,"enabled":true,"agents":["pi"],"profiles":{"pi":{"model":"anthropic-vertex/claude-opus-4-8","fast":true}}}`,
	} {
		if err := cfg.ApplyReviewFixRunConfig(encoded); err == nil || !strings.Contains(err.Error(), "restore review fixer") {
			t.Fatalf("ApplyReviewFixRunConfig(%s) error = %v", encoded, err)
		}
	}
}
