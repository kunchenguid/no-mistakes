package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func writePiCatalogStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, "pi.cmd")
		script := "@echo off\r\necho %* | findstr /c:\"--model openrouter/z-ai/glm-5.3\" >nul\r\nif not errorlevel 1 exit /b 0\r\necho Error: Model \"stub\" not found. Use --list-models to see available models. 1>&2\r\nexit /b 1\r\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	path := filepath.Join(dir, "pi")
	script := `#!/bin/sh
# Emulates Pi's resolver contract: it exits 0 when the --model pattern
# resolves and 1 with its not-found error on stderr when it does not.
model=
prev=
for a in "$@"; do
	[ "$prev" = "--model" ] && model="$a"
	prev="$a"
done
case "$model" in
	""|openrouter/z-ai/glm-5.3)
		exit 0
		;;
	*)
		printf 'Error: Model "%s" not found. Use --list-models to see available models.\n' "$model" >&2
		exit 1
		;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestNewPipelineAgent_ThreadsTheAgentProfile proves the pipeline half of the
// unified tuning layer: agent_config reaches agent construction per agent, so a
// knob the harness cannot express fails the run at setup instead of being
// silently dropped into a review nobody can reproduce.
func TestNewPipelineAgent_ThreadsTheAgentProfile(t *testing.T) {
	cfg := &config.Config{
		Agent:       types.AgentAntigravity,
		AgentConfig: map[string]agentcfg.Profile{"antigravity": {Model: "some-model"}},
	}
	_, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), t.TempDir(), fakeLookPath, runenv.Overlay{})
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
	ag, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), t.TempDir(), fakeLookPath, runenv.Overlay{})
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
	ag, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err != nil {
		t.Fatalf("newPipelineAgent = %v", err)
	}
	_ = ag.Close()
}

func TestNewPipelineAgent_RejectsUnknownPiAgentConfigModelBeforeExecution(t *testing.T) {
	bin := writePiCatalogStub(t)
	cfg := &config.Config{
		Agent:             types.AgentPi,
		AgentPathOverride: map[string]string{"pi": bin},
		AgentConfig:       map[string]agentcfg.Profile{"pi": {Model: "openrouter/stealth/ox-alpha"}},
	}

	_, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err == nil {
		t.Fatal("unknown Pi model must fail pipeline setup")
	}
	for _, want := range []string{"openrouter/stealth/ox-alpha", "agent_config.pi.model"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("setup error = %q, want %q", err, want)
		}
	}
}

func TestNewPipelineAgent_RejectsUnknownPiArgsOverrideModelWithItsSource(t *testing.T) {
	bin := writePiCatalogStub(t)
	cfg := &config.Config{
		Agent:             types.AgentPi,
		AgentPathOverride: map[string]string{"pi": bin},
		AgentArgsOverride: map[string][]string{"pi": {"--model", "openrouter/stealth/ox-alpha"}},
	}

	_, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err == nil {
		t.Fatal("unknown Pi override model must fail pipeline setup")
	}
	for _, want := range []string{"openrouter/stealth/ox-alpha", "agent_args_override.pi"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("setup error = %q, want %q", err, want)
		}
	}
}

func TestNewPipelineAgent_AcceptsPiAgentConfigModelPresentInCatalogue(t *testing.T) {
	bin := writePiCatalogStub(t)
	cfg := &config.Config{
		Agent:             types.AgentPi,
		AgentPathOverride: map[string]string{"pi": bin},
		AgentConfig:       map[string]agentcfg.Profile{"pi": {Model: "openrouter/z-ai/glm-5.3"}},
	}

	ag, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err != nil {
		t.Fatalf("catalogued Pi model rejected: %v", err)
	}
	_ = ag.Close()
}

func TestNewPipelineAgent_RejectsUnknownPiSettingsDefaultBeforeExecution(t *testing.T) {
	bin := writePiCatalogStub(t)
	agentDir := t.TempDir()
	settingsPath := filepath.Join(agentDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"defaultProvider":"openrouter","defaultModel":"stealth/ox-alpha"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Agent:             types.AgentPi,
		AgentPathOverride: map[string]string{"pi": bin},
	}

	_, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), t.TempDir(), fakeLookPath, runenv.Overlay{
		Set: map[string]string{"PI_CODING_AGENT_DIR": agentDir},
	})
	if err == nil {
		t.Fatal("unknown Pi settings default must fail pipeline setup")
	}
	for _, want := range []string{"stealth/ox-alpha", settingsPath} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("setup error = %q, want %q", err, want)
		}
	}
}

// The probe must consult the run worktree, not just the global agent dir: a
// repo can pin its pipeline model in .pi/settings.json. The rejection needs a
// trust decision Pi would honor, so the agent dir records defaultProjectTrust.
func TestNewPipelineAgent_RejectsUnknownPiProjectSettingsDefaultBeforeExecution(t *testing.T) {
	bin := writePiCatalogStub(t)
	workDir := t.TempDir()
	projectSettings := filepath.Join(workDir, ".pi", "settings.json")
	if err := os.MkdirAll(filepath.Dir(projectSettings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectSettings, []byte(`{"defaultProvider":"openrouter","defaultModel":"stealth/ox-alpha"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	agentDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(`{"defaultProjectTrust":"always"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Agent:             types.AgentPi,
		AgentPathOverride: map[string]string{"pi": bin},
	}

	_, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), workDir, fakeLookPath, runenv.Overlay{
		Set: map[string]string{"PI_CODING_AGENT_DIR": agentDir},
	})
	if err == nil {
		t.Fatal("unknown Pi project settings default must fail pipeline setup")
	}
	for _, want := range []string{"stealth/ox-alpha", projectSettings} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("setup error = %q, want %q", err, want)
		}
	}
}
