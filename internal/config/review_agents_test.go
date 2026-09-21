package config

import (
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"testing"
)

func TestReviewAgentsProfilesAreIndependent(t *testing.T) {
	global := writeGlobalConfig(t, `agent: codex
agent_config:
  pi: {model: default-model, effort: high}
review_agents:
  reviewer: {agent: pi, model: review-model, effort: max}
  fixer: {agent: pi, model: fix-model}
`)
	cfg := Merge(global, &RepoConfig{})
	reviewer := cfg.ForReviewAgent(cfg.ReviewAgents["reviewer"])
	fixer := cfg.ForReviewAgent(cfg.ReviewAgents["fixer"])
	if got := reviewer.AgentProfile(); got != (agentcfg.Profile{Model: "review-model", Effort: agentcfg.EffortMax}) {
		t.Fatalf("reviewer = %+v", got)
	}
	if got := fixer.AgentProfile(); got != (agentcfg.Profile{Model: "fix-model", Effort: agentcfg.EffortHigh}) {
		t.Fatalf("fixer = %+v", got)
	}
	if cfg.Agent != types.AgentCodex || cfg.AgentProfileFor(types.AgentPi).Model != "default-model" {
		t.Fatal("role selection mutated default configuration")
	}
}

func TestReviewAgentsRejectInvalidConfig(t *testing.T) {
	for _, input := range []string{
		"review_agents: {other: {agent: pi}}",
		"review_agents: {reviewer: {model: x}}",
		"review_agents: {reviewer: {agent: auto}}",
		"review_agents: {fixer: {agent: unknown}}",
		"review_agents: {reviewer: {agent: pi, effort: turbo}}",
		"review_agents: {fixer: {agent: cursor, effort: max}}",
		"review_agents: {fixer: {agent: rovodev, model: x}}",
		"review_agents: {reviewer: {agent: pi, typo: x}}",
	} {
		t.Run(input, func(t *testing.T) { loadGlobalConfigError(t, input) })
	}
}

func TestRepositoryCannotSelectReviewAgents(t *testing.T) {
	repo, err := LoadRepoFromBytes([]byte("review_agents: {reviewer: {agent: pi, model: attacker-model}}"))
	if err != nil {
		t.Fatal(err)
	}
	global := writeGlobalConfig(t, "review_agents: {reviewer: {agent: pi, model: operator-model}}")
	cfg := Merge(global, repo)
	if cfg.ReviewAgents["reviewer"].Model != "operator-model" {
		t.Fatal("repository changed operator profile")
	}
	if Merge(DefaultGlobalConfig(), repo).ReviewAgents != nil {
		t.Fatal("repository selected a role")
	}
}

func TestReviewAgentsOmitted(t *testing.T) {
	cfg := Merge(writeGlobalConfig(t, "agent: pi\n"), &RepoConfig{})
	if cfg.ReviewAgents != nil {
		t.Fatalf("unexpected roles: %+v", cfg.ReviewAgents)
	}
}

func TestReviewAgentsLaterRoundOverlaysParse(t *testing.T) {
	global := writeGlobalConfig(t, `agent: codex
agent_config:
  pi: {model: default-model, effort: high}
review_agents:
  fixer: {agent: pi, model: strong-model, effort: max}
  fixer_after_round: {agent: pi, model: cheap-model, after_round: 3}
  reviewer_after_round: {agent: pi, model: late-review-model}
`)
	cfg := Merge(global, &RepoConfig{})
	if got := cfg.ForReviewAgent(cfg.ReviewAgents[RoleFixerAfterRound]).AgentProfile(); got != (agentcfg.Profile{Model: "cheap-model", Effort: agentcfg.EffortHigh}) {
		t.Fatalf("late fixer = %+v", got)
	}
	// after_round: 3 keeps rounds 1-3 on the base role and hands round 4 over.
	if got := cfg.ReviewAgentTakeoverRound(RoleFixerAfterRound); got != 4 {
		t.Fatalf("fixer takeover round = %d, want 4", got)
	}
	// An overlay without after_round takes over from the second round.
	if got := cfg.ReviewAgentTakeoverRound(RoleReviewerAfterRound); got != 2 {
		t.Fatalf("reviewer takeover round = %d, want 2", got)
	}
	// An unconfigured overlay must report no takeover at all, which is what
	// keeps every round on the base role for operators who set nothing.
	if got := cfg.ReviewAgentTakeoverRound(RoleReviewer); got != 0 {
		t.Fatalf("unset overlay takeover round = %d, want 0", got)
	}
}

func TestReviewAgentsRejectInvalidLaterRoundOverlays(t *testing.T) {
	for _, input := range []string{
		"review_agents: {fixer: {agent: pi, after_round: 2}}",
		"review_agents: {reviewer: {agent: pi, after_round: 1}}",
		"review_agents: {fixer_after_round: {agent: pi, after_round: -1}}",
		// An explicit zero is below the documented minimum, so it must fail
		// closed rather than read as the unset default of 1.
		"review_agents: {fixer_after_round: {agent: pi, after_round: 0}}",
		"review_agents: {fixer: {agent: pi, after_round: 0}}",
		"review_agents: {reviewer_after_round: {model: x}}",
		"review_agents: {fixer_after_round: {agent: auto}}",
		"review_agents: {fixer_after_rounds: {agent: pi}}",
	} {
		t.Run(input, func(t *testing.T) { loadGlobalConfigError(t, input) })
	}
}
