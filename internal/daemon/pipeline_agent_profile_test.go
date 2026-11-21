package daemon

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func ompLookPath(string) (string, error) { return "/usr/bin/omp", nil }

func TestNewPipelineAgentThreadsTheOMPProfile(t *testing.T) {
	cfg := &config.Config{
		Agent:       types.AgentOMP,
		AgentConfig: map[string]agentcfg.Profile{"omp": {Model: "dashscope/qwen4-max", Effort: agentcfg.EffortHigh}},
	}
	ag, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), ompLookPath, runenv.Overlay{})
	if err != nil {
		t.Fatalf("newPipelineAgent: %v", err)
	}
	_ = ag.Close()
}

func TestNewPipelineAgentRejectsUnsupportedAgent(t *testing.T) {
	cfg := &config.Config{Agent: types.AgentName("claude")}
	if _, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), ompLookPath, runenv.Overlay{}); err == nil {
		t.Fatal("an unsupported harness must fail setup")
	}
}

func TestNewPipelineAgentNoProfileIsUnchanged(t *testing.T) {
	cfg := &config.Config{Agent: types.AgentOMP}
	ag, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), ompLookPath, runenv.Overlay{})
	if err != nil {
		t.Fatalf("newPipelineAgent: %v", err)
	}
	_ = ag.Close()
}
