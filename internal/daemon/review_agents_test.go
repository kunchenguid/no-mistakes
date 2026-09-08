package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPipelineReviewRolesUseIndependentPiProfiles(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "pi")
	const response = `{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"ok"}]}]}`
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > pi-argv.txt\ncat >/dev/null\nprintf '%s\\n' '" + response + "'\n"
	if runtime.GOOS == "windows" {
		bin += ".cmd"
		script = "@echo off\r\necho %* > pi-argv.txt\r\nmore > nul\r\necho " + response + "\r\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	global, err := config.LoadGlobalFromBytes([]byte(`agent: pi
agent_config:
  pi: {model: default-model, effort: high}
review_agents:
  reviewer: {agent: pi, model: anthropic-vertex/claude-opus-4-8, effort: max}
  fixer: {agent: pi, model: google-vertex/gemini-3.8-flash, effort: max}
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Merge(global, &config.RepoConfig{})
	cfg.AgentPathOverride = map[string]string{"pi": bin}
	cfg.DisableProjectSettings = true
	ag, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	for _, tc := range []struct{ purpose, model, effort string }{
		{"review", "anthropic-vertex/claude-opus-4-8", "max"},
		{"review-fix", "google-vertex/gemini-3.8-flash", "max"},
		{"review", "anthropic-vertex/claude-opus-4-8", "max"},
		{"test-evidence", "default-model", "high"},
	} {
		_, err := ag.Run(context.Background(), agent.RunOpts{Purpose: tc.purpose, Prompt: "hello", CWD: dir})
		if err != nil {
			t.Fatal(err)
		}
		args, err := os.ReadFile(filepath.Join(dir, "pi-argv.txt"))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"--model " + tc.model, "--thinking " + tc.effort, "--no-context-files"} {
			if !strings.Contains(string(args), want) {
				t.Fatalf("%s args %q missing %q", tc.purpose, args, want)
			}
		}
	}
}

func TestPipelineReviewRoleFailsClosed(t *testing.T) {
	cfg := &config.Config{Agent: types.AgentPi, DisableProjectSettings: true,
		ReviewAgents: map[string]config.ReviewAgent{"reviewer": {Agent: types.AgentAntigravity}}}
	_, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err == nil || !strings.Contains(err.Error(), "review_agents.reviewer") || !strings.Contains(err.Error(), "does not neutralize") {
		t.Fatalf("unsafe reviewer error = %v", err)
	}
}
