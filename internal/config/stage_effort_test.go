package config

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestStageEffortConfig(t *testing.T) {
	global := writeGlobalConfig(t, `agent: pi
agent_config:
  pi:
    model: unchanged-model
    effort: medium
stage_effort:
  pi:
    review: high
    review-fix: medium
    lint: low
`)
	repo, err := LoadRepoFromBytes([]byte("stage_effort:\n  pi:\n    review: low\nallow_repo_commands: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(global, repo)
	if cfg.StageEffort["pi"]["review"] != agentcfg.EffortHigh {
		t.Fatal("repository changed operator effort")
	}
	if cfg.AgentProfile().Model != "unchanged-model" || cfg.AgentProfile().Effort != agentcfg.EffortMedium {
		t.Fatal("stage override changed global profile")
	}
	if got := writeGlobalConfig(t, "agent: pi\n").StageEffort; len(got) != 0 {
		t.Fatalf("default = %v", got)
	}
}

func TestStageEffortInvalidConfig(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"pi:\n    push: high", "invalid stage"},
		{"pi:\n    reviewer: high", "invalid stage"},
		{"pi:\n    review-fixer: high", "invalid stage"},
		{"pi:\n    review: turbo", "invalid effort"},
		{"pi:\n    review: ''", "non-empty"},
		{"pi:\n    review: {model: x}", "unmarshal"},
		{"missing:\n    review: high", "unknown agent"},
		{"rovodev:\n    review: high", "cannot express effort"},
		{"antigravity:\n    test: high", "cannot express effort"},
		{"cursor:\n    lint: high", "cannot express effort"},
		{"acp:custom:\n    ci: high", "cannot express effort"},
	} {
		t.Run(tc.yaml, func(t *testing.T) {
			if err := loadGlobalConfigError(t, "stage_effort:\n  "+tc.yaml+"\n"); !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCombineHousekeepingEffortPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stages  agentcfg.StageEfforts
		raw     []string
		command string
		want    bool
	}{
		{name: "default", want: true},
		{name: "equal", stages: agentcfg.StageEfforts{"document": "high", "lint": "high"}, want: true},
		{name: "different", stages: agentcfg.StageEfforts{"document": "high", "lint": "low"}},
		{name: "inherited equal", stages: agentcfg.StageEfforts{"lint": "medium"}, want: true},
		{name: "inherited different", stages: agentcfg.StageEfforts{"lint": "high"}},
		{name: "raw wins", stages: agentcfg.StageEfforts{"document": "high", "lint": "low"}, raw: []string{"--thinking", "medium"}, want: true},
		{name: "raw equals wins", stages: agentcfg.StageEfforts{"lint": "high"}, raw: []string{"--thinking=low"}, want: true},
		{name: "command", command: "echo lint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Agent: types.AgentPi, AgentConfig: map[string]agentcfg.Profile{"pi": {Effort: "medium"}}, StageEffort: map[string]agentcfg.StageEfforts{"pi": tc.stages}, AgentArgsOverride: map[string][]string{"pi": tc.raw}, Commands: Commands{Lint: tc.command}}
			if got := cfg.CombineHousekeeping(); got != tc.want {
				t.Fatalf("combine = %v, want %v", got, tc.want)
			}
		})
	}
	cfg := &Config{Agents: []types.AgentName{types.AgentPi, types.AgentCodex}, StageEffort: map[string]agentcfg.StageEfforts{"codex": {"lint": "high"}}}
	if cfg.CombineHousekeeping() {
		t.Fatal("fallback provider effort ignored")
	}
}
