package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestNewPipelineAgent_ThreadsTheAgentProfile proves the pipeline half of the
// unified tuning layer: agent_config reaches agent construction per agent, so a
// knob the harness cannot express fails the run at setup instead of being
// silently dropped into a review nobody can reproduce.
func TestNewPipelineAgent_ThreadsTheAgentProfile(t *testing.T) {
	cfg := &config.Config{
		Agent:       types.AgentAntigravity,
		AgentConfig: map[string]agentcfg.Profile{"antigravity": {Model: "some-model"}},
	}
	_, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err == nil {
		t.Fatal("a model on a harness that cannot express one must fail setup")
	}
	if !strings.Contains(err.Error(), "cannot express model") {
		t.Fatalf("refusal should explain the reason, got: %v", err)
	}
}

func TestNewPipelineAgent_ProfileIsPerAgent(t *testing.T) {
	cfg := &config.Config{
		Agents:      []types.AgentName{types.AgentClaude, types.AgentAntigravity},
		AgentConfig: map[string]agentcfg.Profile{"claude": {Model: "sonnet", Effort: agentcfg.EffortHigh}},
	}
	ag, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err != nil {
		t.Fatalf("a profile set only for claude must not reach antigravity: %v", err)
	}
	_ = ag.Close()
}

// TestNewPipelineAgent_NoProfileIsUnchanged is the backwards-compatibility
// floor at the daemon: a configuration that predates agent_config builds every
// agent exactly as before.
func TestNewPipelineAgent_NoProfileIsUnchanged(t *testing.T) {
	cfg := &config.Config{
		Agent:             types.AgentCodex,
		AgentArgsOverride: map[string][]string{"codex": {"-m", "gpt-5.4"}},
	}
	ag, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err != nil {
		t.Fatalf("newPipelineAgent = %v", err)
	}
	_ = ag.Close()
}
