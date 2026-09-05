package config

import (
	"slices"
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
		ReviewFixAgents: []types.AgentName{types.AgentCodex},
		ReviewFixAgentConfig: map[string]ReviewFixProfile{
			"codex": {Profile: agentcfg.Profile{Model: "changed"}},
		},
	}
	if err := recovered.ApplyReviewFixRunConfig(encoded); err != nil {
		t.Fatal(err)
	}
	if len(recovered.ReviewFixAgents) != 1 || recovered.ReviewFixAgents[0] != types.AgentPi {
		t.Fatalf("restored fixer = %v", recovered.ReviewFixAgents)
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
		Agent:  types.AgentPi,
		Agents: []types.AgentName{types.AgentPi},
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

func TestReviewFixRunConfigPinsInheritedSelectionAndProfile(t *testing.T) {
	started := &Config{
		Agent:  types.AgentCodex,
		Agents: []types.AgentName{types.AgentCodex},
		AgentConfig: map[string]agentcfg.Profile{
			"codex": {Model: "gpt-5.3-codex", Effort: agentcfg.EffortHigh},
		},
		AgentArgsOverride: map[string][]string{
			"codex": {"--model", "raw-fixer-model", "--reasoning", "low"},
		},
	}
	encoded, err := started.MarshalReviewFixRunConfig()
	if err != nil {
		t.Fatal(err)
	}

	recovered := &Config{
		Agent:             types.AgentPi,
		Agents:            []types.AgentName{types.AgentPi},
		AgentConfig:       map[string]agentcfg.Profile{"codex": {Model: "changed"}},
		AgentArgsOverride: map[string][]string{"codex": {"--thinking", "high"}},
		ReviewFixAgents:   []types.AgentName{types.AgentPi},
		ReviewFixAgentConfig: map[string]ReviewFixProfile{
			"pi": {Profile: agentcfg.Profile{Model: "openai-codex/gpt-5.6-sol"}, Fast: true},
		},
	}
	if err := recovered.ApplyReviewFixRunConfig(encoded); err != nil {
		t.Fatal(err)
	}
	if len(recovered.ReviewFixAgents) != 1 || recovered.ReviewFixAgents[0] != types.AgentCodex {
		t.Fatalf("restored inherited fixer = %v", recovered.ReviewFixAgents)
	}
	got, explicit := recovered.ReviewFixProfileFor(types.AgentCodex)
	want := agentcfg.Profile{Model: "gpt-5.3-codex", Effort: agentcfg.EffortHigh}
	if !explicit || got.Profile != want || got.Fast {
		t.Fatalf("restored inherited profile = %#v, explicit=%v, want %#v", got, explicit, want)
	}
	wantArgs := []string{"--model", "raw-fixer-model", "--reasoning", "low"}
	if gotArgs := recovered.ReviewFixAgentArgsFor(types.AgentCodex); !slices.Equal(gotArgs, wantArgs) {
		t.Fatalf("restored fixer args = %v, want %v", gotArgs, wantArgs)
	}
	if gotArgs := recovered.AgentArgsFor(types.AgentCodex); !slices.Equal(gotArgs, []string{"--thinking", "high"}) {
		t.Fatalf("ordinary agent args changed to %v", gotArgs)
	}
}

func TestApplyReviewFixRunConfigFailsClosedOnInvalidStoredShape(t *testing.T) {
	cfg := &Config{}
	for _, encoded := range []string{
		`{"version":3,"agents":["pi"],"args":{"pi":null}}`,
		`{"version":2,"agents":["pi"]}`,
		`{"version":2,"agents":["pi"],"args":{},"profiles":{"pi":{}}}`,
		`{"version":2,"agents":["pi"],"args":{"pi":null},"profiles":{"pi":{"model":"anthropic-vertex/claude-opus-4-8","fast":true}}}`,
	} {
		if err := cfg.ApplyReviewFixRunConfig(encoded); err == nil || !strings.Contains(err.Error(), "restore review fixer") {
			t.Fatalf("ApplyReviewFixRunConfig(%s) error = %v", encoded, err)
		}
	}
}
