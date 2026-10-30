package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
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
		"review_agents: {fixer: {agent: pi, fallback: [{agent: codex}]}}",
		"review_agents: {reviewer: {agent: pi, fallback: [{agent: auto}]}}",
		"review_agents: {reviewer: {agent: pi, fallback: [{agent: codex, effort: turbo}]}}",
		"review_agents: {reviewer: {agent: pi, fallback: [{agent: codex, after_round: 1}]}}",
		"review_agents: {reviewer: {agent: pi, fallback: [{agent: codex, fallback: [{agent: pi}]}]}}",
	} {
		t.Run(input, func(t *testing.T) { loadGlobalConfigError(t, input) })
	}
}

func TestReviewAgentFallbackProfilesStayOperatorOwned(t *testing.T) {
	global := writeGlobalConfig(t, `agent: codex
agent_config:
  pi: {model: default-model, effort: high}
review_agents:
  reviewer:
    agent: codex
    fallback: [{agent: pi, model: reviewer-model}]
`)
	repo, err := LoadRepoFromBytes([]byte(`review_agents: {reviewer: {agent: pi, fallback: [{agent: pi, model: attacker-model}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(global, repo)
	fallback := cfg.ForReviewAgent(cfg.ReviewAgents[RoleReviewer].Fallback[0])
	if got := fallback.AgentProfile(); got != (agentcfg.Profile{Model: "reviewer-model", Effort: agentcfg.EffortHigh}) {
		t.Fatalf("fallback profile = %+v", got)
	}
	if cfg.AgentProfileFor(types.AgentPi).Model != "default-model" {
		t.Fatal("fallback mutated the shared profile")
	}
	if err := validatePiProfileAgents(types.AgentPi, nil, map[string]ReviewAgent{RoleReviewer: {
		Agent: types.AgentPi, Fallback: []ReviewAgent{{Agent: types.AgentCodex}},
	}}); err == nil {
		t.Fatal("Pi run pin silently dropped a non-Pi fallback")
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

func TestRepositoryOverrideReviewAgentsReplacePerRole(t *testing.T) {
	global := writeGlobalConfig(t, `agent: claude
review_agents:
  reviewer: {agent: claude, model: global-review}
  fixer: {agent: codex}
  reviewer_after_round: {agent: codex}
repository_overrides:
  https://github.com/acme/partial:
    review_agents:
      reviewer: {agent: opencode, model: openrouter/z-ai/glm-4.6}
  https://github.com/acme/full:
    review_agents:
      reviewer: {agent: pi, model: r}
      fixer: {agent: pi, model: f}
      reviewer_after_round: {agent: pi, model: rl, after_round: 2}
      fixer_after_round: {agent: pi, model: fl}
  https://github.com/acme/none:
    test: {instructions: run it}
`)
	afterRound := 2
	want := map[string]map[string]ReviewAgent{
		"https://github.com/acme/other": global.ReviewAgents,
		"https://github.com/acme/none":  global.ReviewAgents,
		"git@github.com:acme/partial.git": {
			RoleReviewer:           {Agent: types.AgentOpenCode, Model: "openrouter/z-ai/glm-4.6"},
			RoleFixer:              {Agent: types.AgentCodex},
			RoleReviewerAfterRound: {Agent: types.AgentCodex},
		},
		"https://github.com/acme/full": {
			RoleReviewer:           {Agent: types.AgentPi, Model: "r"},
			RoleFixer:              {Agent: types.AgentPi, Model: "f"},
			RoleReviewerAfterRound: {Agent: types.AgentPi, Model: "rl", AfterRound: &afterRound},
			RoleFixerAfterRound:    {Agent: types.AgentPi, Model: "fl"},
		},
	}
	for remote, roles := range want {
		t.Run(remote, func(t *testing.T) {
			// The repository's own config must not reach review_agents either way.
			repo, err := LoadRepoFromBytes([]byte("review_agents: {reviewer: {agent: pi, model: attacker-model}}"))
			if err != nil {
				t.Fatal(err)
			}
			if got := MergeForRemote(global, repo, remote).ReviewAgents; !reflect.DeepEqual(got, roles) {
				t.Fatalf("review agents = %+v, want %+v", got, roles)
			}
		})
	}
	if got := global.ReviewAgents[RoleReviewer]; got.Agent != types.AgentClaude || len(global.ReviewAgents) != 3 {
		t.Fatalf("repository override mutated the global roles: %+v", global.ReviewAgents)
	}
}

func TestRepositoryOverrideReviewAgentsRejectInvalidConfig(t *testing.T) {
	for input, want := range map[string]string{
		"{other: {agent: pi}}":                   `invalid review_agents role "other"`,
		"{fixer: {agent: unknown}}":              `review_agents.fixer.agent must name an explicit harness, got "unknown"`,
		"{reviewer: {agent: pi, effort: turbo}}": "invalid review_agents.reviewer:",
	} {
		t.Run(input, func(t *testing.T) {
			err := loadGlobalConfigError(t, "repository_overrides:\n  https://github.com/acme/widget:\n    review_agents: "+input+"\n")
			if !strings.Contains(err.Error(), "repository_overrides.github.com/acme/widget") || !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want %q", err, want)
			}
		})
	}
}
