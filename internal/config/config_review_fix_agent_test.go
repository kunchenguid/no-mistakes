package config

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLoadGlobal_ReviewFixAgentSupportsExistingAgentSelectionForms(t *testing.T) {
	cfg := writeGlobalConfig(t, `
agent: claude
review_fix_agent: [pi, codex]
`)
	if cfg.ReviewFixAgent != types.AgentPi {
		t.Fatalf("ReviewFixAgent = %q, want pi", cfg.ReviewFixAgent)
	}
	if len(cfg.ReviewFixAgents) != 2 || cfg.ReviewFixAgents[0] != types.AgentPi || cfg.ReviewFixAgents[1] != types.AgentCodex {
		t.Fatalf("ReviewFixAgents = %v, want [pi codex]", cfg.ReviewFixAgents)
	}

	err := loadGlobalConfigError(t, "review_fix_agent:\n  nested: value\n")
	if !strings.Contains(err.Error(), "review_fix_agent must be a string or a list of strings") {
		t.Fatalf("error = %v, want field-specific correction", err)
	}
}

// TestMerge_ReviewFixAgentGlobalOnlyAndIndependentOfRepoAgent pins both
// precedence boundaries: a trusted repo can still select the ordinary
// pipeline agent, while only the operator's global config can select the
// credentialed process used to remediate Review findings.
func TestMerge_ReviewFixAgentGlobalOnlyAndIndependentOfRepoAgent(t *testing.T) {
	global := DefaultGlobalConfig()
	global.Agent = types.AgentClaude
	global.Agents = []types.AgentName{types.AgentClaude}
	global.ReviewFixAgent = types.AgentPi
	global.ReviewFixAgents = []types.AgentName{types.AgentPi}

	repo, err := LoadRepoFromBytes([]byte("agent: codex\nreview_fix_agent: antigravity\n"))
	if err != nil {
		t.Fatal(err)
	}
	merged := Merge(global, repo)
	if merged.Agent != types.AgentCodex {
		t.Fatalf("Agent = %q, want trusted repo override codex", merged.Agent)
	}
	if merged.ReviewFixAgent != types.AgentPi || len(merged.ReviewFixAgents) != 1 || merged.ReviewFixAgents[0] != types.AgentPi {
		t.Fatalf("Review fixer = %q %v, want global pi", merged.ReviewFixAgent, merged.ReviewFixAgents)
	}
}

// TestMerge_ReviewFixAgentAbsentKeepsLegacyFallback is the migration floor for
// every config written before review_fix_agent existed. The absence survives
// parsing and merging, so the pipeline can use the effective agent unchanged.
func TestMerge_ReviewFixAgentAbsentKeepsLegacyFallback(t *testing.T) {
	global := writeGlobalConfig(t, "agent: claude\n")
	merged := Merge(global, &RepoConfig{Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex}})
	if merged.HasReviewFixAgentOverride() {
		t.Fatalf("unexpected Review-fixer override: %q %v", merged.ReviewFixAgent, merged.ReviewFixAgents)
	}
}

func TestResolveReviewFixAgentUsesAgentAvailabilitySemantics(t *testing.T) {
	cfg := &Config{
		ReviewFixAgent:  types.AgentPi,
		ReviewFixAgents: []types.AgentName{types.AgentPi, types.AgentCodex},
	}
	lookPath := func(name string) (string, error) {
		if name == "codex" {
			return "/bin/codex", nil
		}
		return "", exec.ErrNotFound
	}
	if err := cfg.ResolveReviewFixAgent(t.Context(), lookPath); err != nil {
		t.Fatal(err)
	}
	if cfg.ReviewFixAgent != types.AgentCodex || len(cfg.ReviewFixAgents) != 1 {
		t.Fatalf("resolved Review fixer = %q %v, want codex", cfg.ReviewFixAgent, cfg.ReviewFixAgents)
	}

	unavailable := &Config{ReviewFixAgent: types.AgentPi}
	err := unavailable.ResolveReviewFixAgent(t.Context(), lookPath)
	if err == nil || !strings.Contains(err.Error(), "no runnable agent found for configured review_fix_agent") {
		t.Fatalf("error = %v, want concise review_fix_agent guidance", err)
	}

	unknown := &Config{ReviewFixAgent: types.AgentName("unknown")}
	err = unknown.ResolveReviewFixAgent(t.Context(), lookPath)
	if err == nil || !strings.Contains(err.Error(), `set "review_fix_agent" in ~/.no-mistakes/config.yaml`) {
		t.Fatalf("error = %v, want self-correcting review_fix_agent help", err)
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
	if cfg.Agent != types.AgentPi || cfg.ReviewFixAgent != "" {
		t.Fatalf("agent pairing = %q/%q, want pi with a role profile", cfg.Agent, cfg.ReviewFixAgent)
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
