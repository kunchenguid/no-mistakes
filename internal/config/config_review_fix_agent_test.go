package config

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLoadGlobal_RejectsRemovedReviewFixAgent(t *testing.T) {
	err := loadGlobalConfigError(t, "review_fix_agent: codex\n")
	if !strings.Contains(err.Error(), "review_fix_agent") {
		t.Fatalf("error = %v, want removed field name", err)
	}
}

func TestLoadGlobal_CaptainReviewAndFixPairing(t *testing.T) {
	cfg := writeGlobalConfig(t, `
agent: pi
agent_config:
  pi:
    model: anthropic-vertex/claude-opus-4-8
    effort: xhigh
    review_fix:
      model: openai-codex/gpt-5.6-sol
      effort: low
      fast: true
`)
	if cfg.Agent != types.AgentPi {
		t.Fatalf("agent = %q, want pi with a role profile", cfg.Agent)
	}
	reviewer := agentcfg.Profile{Model: "anthropic-vertex/claude-opus-4-8", Effort: agentcfg.EffortXHigh}
	if got := cfg.AgentConfig["pi"]; got != reviewer {
		t.Fatalf("Pi reviewer profile = %#v, want %#v", got, reviewer)
	}
	fixer, ok := cfg.ReviewFixAgentConfig["pi"]
	wantFixer := ReviewFixProfile{
		Profile: agentcfg.Profile{Model: "openai-codex/gpt-5.6-sol", Effort: agentcfg.EffortLow},
		Fast:    true,
	}
	if !ok || fixer != wantFixer {
		t.Fatalf("Pi fixer profile = %#v, want %#v", fixer, wantFixer)
	}
}

func TestLoadGlobal_ReviewFixProfileFastValidation(t *testing.T) {
	for _, tt := range []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "non Pi",
			yaml: "agent_config:\n  codex:\n    review_fix:\n      model: gpt-5.6-sol\n      fast: true\n",
			want: "fast is supported only by agent pi",
		},
		{
			name: "non Codex provider",
			yaml: "agent_config:\n  pi:\n    review_fix:\n      model: anthropic-vertex/claude-opus-4-8\n      fast: true\n",
			want: "fast requires model: openai-codex/<model>",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := loadGlobalConfigError(t, tt.yaml)
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}
