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
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// fakeLookPath makes every probed agent binary resolve, so agent resolution is
// deterministic and independent of what is installed on the test host.
func fakeLookPath(bin string) (string, error) { return "/fake/bin/" + bin, nil }

func fakeCapablePi(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	name := "pi"
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 0.80.10; exit 0; fi\nif [ \"$1\" = \"--help\" ]; then echo --mode --no-session --no-extensions --no-skills --no-prompt-templates --no-themes --no-context-files --no-approve --system-prompt --append-system-prompt; exit 0; fi\n"
	if runtime.GOOS == "windows" {
		name = "pi.cmd"
		script = "@echo off\r\nif \"%~1\"==\"--version\" (echo 0.80.10& exit /b 0)\r\nif \"%~1\"==\"--help\" (echo --mode --no-session --no-extensions --no-skills --no-prompt-templates --no-themes --no-context-files --no-approve --system-prompt --append-system-prompt& exit /b 0)\r\n"
	}
	piBin := filepath.Join(dir, name)
	if err := os.WriteFile(piBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pi: %v", err)
	}
	return piBin
}

// TestNewPipelineAgent_OptOut_AdmitsVerifiedHarness proves that under the trusted
// opt-out (disable_project_settings=true), a verified harness passes the gate and
// its pipeline agent reports neutralized.
func TestNewPipelineAgent_OptOut_AdmitsVerifiedHarness(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentCodex, types.AgentClaude, types.AgentPi} {
		cfg := &config.Config{Agent: name, DisableProjectSettings: true}
		if name == types.AgentPi {
			cfg.AgentPathOverride = map[string]string{"pi": fakeCapablePi(t)}
		}
		ag, err := newPipelineAgent(context.Background(), cfg, fakeLookPath)
		if err != nil {
			t.Fatalf("%s must pass under opt-out, got: %v", name, err)
		}
		if !agent.NeutralizesGateInstructions(ag) {
			t.Errorf("%s pipeline agent must report neutralized under opt-out", name)
		}
		_ = ag.Close()
	}
}

// TestNewPipelineAgent_OptOut_RefusesUnverifiedHarness is the captain-mandated
// fail-closed contract at the daemon wiring: under the opt-out, a harness with no
// verified neutralization knob is refused rather than launched with project
// instructions loaded.
func TestNewPipelineAgent_OptOut_RefusesUnverifiedHarness(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentOpenCode, types.AgentCopilot} {
		cfg := &config.Config{Agent: name, DisableProjectSettings: true}
		if _, err := newPipelineAgent(context.Background(), cfg, fakeLookPath); err == nil {
			t.Fatalf("%s must be refused under opt-out", name)
		} else if !strings.Contains(err.Error(), "does not neutralize") || !strings.Contains(err.Error(), string(name)) {
			t.Errorf("%s refusal should name the harness and reason, got: %v", name, err)
		}
	}
}

// TestNewPipelineAgent_NoOptOut_AdmitsEveryHarness is the backward-compat
// guarantee: when the repo did NOT opt out, every harness - including ones with
// no suppression knob - is admitted and runs exactly as before.
func TestNewPipelineAgent_NoOptOut_AdmitsEveryHarness(t *testing.T) {
	// rovodev is omitted: its resolution runs a real version probe that a fake
	// binary path cannot satisfy. opencode/pi/copilot already prove that an
	// unverified adapter is admitted when the repo did not opt out.
	for _, name := range []types.AgentName{types.AgentCodex, types.AgentClaude, types.AgentOpenCode, types.AgentPi, types.AgentCopilot} {
		cfg := &config.Config{Agent: name} // DisableProjectSettings defaults false
		ag, err := newPipelineAgent(context.Background(), cfg, fakeLookPath)
		if err != nil {
			t.Fatalf("%s must be admitted when the repo did not opt out, got: %v", name, err)
		}
		_ = ag.Close()
	}
}

// TestNewPipelineAgent_OptOut_RefusesDefeatedKnob proves the gate fails closed
// even for a verified harness when an operator override defeats its knob.
func TestNewPipelineAgent_OptOut_RefusesDefeatedKnob(t *testing.T) {
	tests := []struct {
		name     types.AgentName
		override []string
	}{
		{name: types.AgentCodex, override: []string{"-c", "project_doc_max_bytes=8192"}},
		{name: types.AgentPi, override: []string{"--extension", "./project.ts"}},
		{name: types.AgentPi, override: []string{"--system-prompt", "project identity"}},
	}
	for _, tt := range tests {
		t.Run(string(tt.name)+"_"+tt.override[0], func(t *testing.T) {
			cfg := &config.Config{
				Agent:                  tt.name,
				DisableProjectSettings: true,
				AgentArgsOverride:      map[string][]string{string(tt.name): tt.override},
			}
			if _, err := newPipelineAgent(context.Background(), cfg, fakeLookPath); err == nil {
				t.Fatalf("%s with override %v must be refused under opt-out", tt.name, tt.override)
			} else if !strings.Contains(err.Error(), "does not neutralize") {
				t.Errorf("refusal should explain the reason, got: %v", err)
			}
		})
	}
}

func TestNewPipelineAgent_OptOut_PiModelThinkingOverrideAllowed(t *testing.T) {
	cfg := &config.Config{
		Agent:                  types.AgentPi,
		DisableProjectSettings: true,
		AgentPathOverride:      map[string]string{"pi": fakeCapablePi(t)},
		AgentArgsOverride: map[string][]string{"pi": {
			"--model", "openai-codex/gpt-5.6-sol", "--thinking", "medium",
		}},
	}
	ag, err := newPipelineAgent(context.Background(), cfg, fakeLookPath)
	if err != nil {
		t.Fatalf("pi model/thinking override must pass under opt-out: %v", err)
	}
	_ = ag.Close()
}

// TestNewPipelineAgent_OptOut_FallbackRefusesAnyUnverifiedMember proves an
// ordered fallback list fails closed under opt-out if any member is unverified.
func TestNewPipelineAgent_OptOut_FallbackRefusesAnyUnverifiedMember(t *testing.T) {
	cfg := &config.Config{Agents: []types.AgentName{types.AgentCodex, types.AgentOpenCode}, DisableProjectSettings: true}
	if _, err := newPipelineAgent(context.Background(), cfg, fakeLookPath); err == nil {
		t.Fatal("a fallback list containing an unverified harness must be refused under opt-out")
	}
	cfg = &config.Config{
		Agents:                 []types.AgentName{types.AgentCodex, types.AgentClaude, types.AgentPi},
		DisableProjectSettings: true,
		AgentPathOverride:      map[string]string{"pi": fakeCapablePi(t)},
	}
	if ag, err := newPipelineAgent(context.Background(), cfg, fakeLookPath); err != nil {
		t.Fatalf("a fallback list of only verified harnesses must pass under opt-out, got: %v", err)
	} else {
		_ = ag.Close()
	}
}
