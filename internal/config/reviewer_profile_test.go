package config

import (
	"reflect"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewerSelectionConcurrentProfilesKeepPrimaryAndFixer(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte(`agent: codex
agent_config:
  codex: {model: writer, effort: medium}
review_agents:
  reviewer: {agent: codex, model: old-reviewer, effort: low}
  reviewer_after_round: {agent: codex, model: late-reviewer, effort: low}
  fixer: {agent: codex, model: fixer, effort: high}
`))
	if err != nil {
		t.Fatal(err)
	}
	pins := []*agentcfg.ReviewerProfile{
		{Agent: types.AgentCodex, Model: "review-a", Effort: agentcfg.EffortHigh},
		{Agent: types.AgentPi, Model: "openai/review-b", Effort: agentcfg.EffortLow},
	}
	var wg sync.WaitGroup
	for _, pin := range pins {
		wg.Go(func() {
			cfg := Merge(global, &RepoConfig{})
			if err := cfg.ApplyReviewerProfile(pin); err != nil {
				t.Error(err)
				return
			}
			role := cfg.ForReviewAgent(cfg.ReviewAgents[RoleReviewer])
			if role.Agent != pin.Agent || role.AgentProfile().Model != pin.Model || role.AgentProfile().Effort != pin.Effort {
				t.Errorf("reviewer drift: %+v", role)
			}
			if cfg.AgentProfile().Model != "writer" || cfg.ReviewAgents[RoleFixer].Model != "fixer" || cfg.ReviewAgentTakeoverRound(RoleReviewerAfterRound) != 0 {
				t.Error("pin changed primary/fixer or retained late reviewer")
			}
		})
	}
	wg.Wait()
	if global.ReviewAgents[RoleReviewer].Model != "old-reviewer" {
		t.Fatal("shared defaults mutated")
	}
}

func TestReviewerSelectionSurvivesPiAndLaterNativeOverrides(t *testing.T) {
	cfg := &Config{Agent: types.AgentPi}
	primary := &agentcfg.PiProfile{Model: "openai/writer", Effort: agentcfg.EffortMedium}
	reviewer := &agentcfg.ReviewerProfile{Agent: types.AgentPi, Model: "openai/reviewer", Effort: agentcfg.EffortHigh}
	if err := cfg.ApplyPiProfile(primary); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ApplyReviewerProfile(reviewer); err != nil {
		t.Fatal(err)
	}
	role := cfg.ForReviewAgent(cfg.ReviewAgents[RoleReviewer])
	if role.AgentProfile().Model != reviewer.Model || !reflect.DeepEqual(role.AgentArgsFor(types.AgentPi), []string{"--provider", "openai"}) {
		t.Fatalf("reviewer did not override primary: %+v", role)
	}
	if cfg.AgentProfile().Model != primary.Model || !reflect.DeepEqual(cfg.AgentArgsFor(types.AgentPi), []string{"--provider", "openai"}) {
		t.Fatal("reviewer changed writer")
	}
}

func TestReviewerSelectionResolvesAndRefusesAmbiguousRequests(t *testing.T) {
	global := &GlobalConfig{AgentConfig: map[string]agentcfg.Profile{"codex": {Model: "reviewer", Effort: agentcfg.EffortHigh}}}
	pin, err := global.ResolveReviewerProfile(&agentcfg.ReviewerProfile{Agent: types.AgentCodex})
	if err != nil || pin.Model != "reviewer" || pin.Effort != agentcfg.EffortHigh {
		t.Fatalf("resolve: %+v %v", pin, err)
	}
	global.AgentArgsOverride = map[string][]string{"codex": {"--model", "other"}}
	if _, err := global.ResolveReviewerProfile(pin); err == nil {
		t.Fatal("ambiguous native override accepted")
	}
	for _, request := range []*agentcfg.ReviewerProfile{{Agent: "auto"}, {Agent: types.AgentAntigravity, Model: "model", Effort: agentcfg.EffortHigh}, {Agent: types.AgentCodex, Model: "https://secret@host/model"}} {
		if err := request.ValidateRequest(); err == nil {
			t.Fatalf("invalid request accepted: %+v", request)
		}
	}
}
