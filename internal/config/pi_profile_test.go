package config

import (
	"reflect"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestResolvePiProfilePrecedenceAndLegacy(t *testing.T) {
	global := &GlobalConfig{Agent: types.AgentPi, AgentConfig: map[string]agentcfg.Profile{"pi": {Model: "anthropic/claude-opus-4-6", Effort: agentcfg.EffortHigh}}}
	pin, err := global.ResolvePiProfile(&agentcfg.PiProfile{Model: "openai-codex/gpt-5.4"})
	if err != nil || pin.Model != "openai-codex/gpt-5.4" || pin.Effort != agentcfg.EffortHigh {
		t.Fatalf("resolve = %+v, %v", pin, err)
	}
	global.AgentArgsOverride = map[string][]string{"pi": {"--thinking", "low"}}
	if _, err := global.ResolvePiProfile(pin); err == nil {
		t.Fatal("raw conflict accepted")
	}
	if pin, err := global.ResolvePiProfile(nil); err != nil || pin != nil {
		t.Fatalf("legacy changed: %+v, %v", pin, err)
	}
	if _, err := (&GlobalConfig{}).ResolvePiProfile(&agentcfg.PiProfile{Effort: agentcfg.EffortHigh}); err == nil {
		t.Fatal("unresolved model accepted")
	}
	complete := &agentcfg.PiProfile{Model: "openai-codex/gpt-5.4", Effort: agentcfg.EffortHigh}
	for _, mixed := range []*GlobalConfig{
		{Agent: types.AgentClaude},
		{Agents: []types.AgentName{types.AgentPi, types.AgentClaude}},
		{Agent: types.AgentPi, ReviewAgents: map[string]ReviewAgent{"reviewer": {Agent: types.AgentClaude}}},
	} {
		if _, err := mixed.ResolvePiProfile(complete); err == nil {
			t.Fatal("mixed harness accepted")
		}
	}
}

func TestApplyPiProfileConcurrentIsolationAndRecovery(t *testing.T) {
	global := &GlobalConfig{
		Agent:             types.AgentClaude,
		AgentConfig:       map[string]agentcfg.Profile{"pi": {Model: "later", Effort: agentcfg.EffortLow}},
		AgentArgsOverride: map[string][]string{"pi": {"--model=later", "--thinking", "low", "--provider", "later", "--no-context-files"}},
		ReviewAgents:      map[string]ReviewAgent{"reviewer": {Agent: types.AgentClaude, Model: "later"}},
	}
	pins := []*agentcfg.PiProfile{{Model: "openai-codex/gpt-5.4", Effort: agentcfg.EffortHigh}, {Model: "anthropic/claude-opus-4-6", Effort: agentcfg.EffortMedium}}
	var wg sync.WaitGroup
	for _, pin := range pins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg := Merge(global, &RepoConfig{})
			cfg.DisableProjectSettings = true
			if err := cfg.ApplyPiProfile(pin); err != nil {
				t.Error(err)
				return
			}
			if cfg.Agent != types.AgentPi || !reflect.DeepEqual(cfg.Agents, []types.AgentName{types.AgentPi}) || cfg.ReviewAgents != nil || !cfg.DisableProjectSettings {
				t.Errorf("pin failed to isolate selection or preserved policy: %+v", cfg)
			}
			if got := cfg.AgentProfile(); got.Model != pin.Model || got.Effort != pin.Effort {
				t.Errorf("profile drift: %+v", got)
			}
			for _, arg := range cfg.AgentArgs() {
				if arg == "later" || arg == "--model=later" {
					t.Error("raw config drift survived")
				}
			}
		}()
	}
	wg.Wait()
	if global.AgentConfig["pi"].Model != "later" || global.AgentArgsOverride["pi"][0] != "--model=later" || global.ReviewAgents["reviewer"].Agent != types.AgentClaude {
		t.Fatal("shared global config mutated")
	}
	legacy := Merge(global, &RepoConfig{})
	if err := legacy.ApplyPiProfile(nil); err != nil || legacy.Agent != types.AgentClaude {
		t.Fatal("legacy behavior changed")
	}
}

func TestPiProfileRefusesMixedHarnesses(t *testing.T) {
	for _, cfg := range []*Config{
		{Agent: types.AgentClaude},
		{Agents: []types.AgentName{types.AgentPi, types.AgentClaude}},
		{Agent: types.AgentPi, ReviewAgents: map[string]ReviewAgent{"reviewer": {Agent: types.AgentClaude}}},
	} {
		if err := cfg.ValidatePiProfileAgents(); err == nil {
			t.Fatal("non-Pi agent accepted")
		}
	}
	cfg := &Config{Agent: types.AgentPi, ReviewAgents: map[string]ReviewAgent{"fixer": {Agent: types.AgentPi, Model: "role-model"}}}
	if err := cfg.ValidatePiProfileAgents(); err != nil {
		t.Fatal(err)
	}
}
