package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// fakeLookPath makes every probed agent binary resolve, so agent resolution is
// deterministic and independent of what is installed on the test host.
func fakeLookPath(bin string) (string, error) { return "/fake/bin/" + bin, nil }

// TestNewPipelineAgent_OptOut_AdmitsVerifiedHarness proves that under the trusted
// opt-out (disable_project_settings=true), a verified harness passes the gate and
// its pipeline agent reports neutralized.
func TestNewPipelineAgent_OptOut_AdmitsVerifiedHarness(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentCodex, types.AgentClaude} {
		cfg := &config.Config{Agent: name, DisableProjectSettings: true}
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
	for _, name := range []types.AgentName{types.AgentOpenCode, types.AgentPi, types.AgentCopilot} {
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
	cfg := &config.Config{
		Agent:                  types.AgentCodex,
		DisableProjectSettings: true,
		AgentArgsOverride:      map[string][]string{"codex": {"-c", "project_doc_max_bytes=8192"}},
	}
	if _, err := newPipelineAgent(context.Background(), cfg, fakeLookPath); err == nil {
		t.Fatal("codex with its knob overridden must be refused under opt-out")
	} else if !strings.Contains(err.Error(), "does not neutralize") {
		t.Errorf("refusal should explain the reason, got: %v", err)
	}
}

// TestNewPipelineAgent_OptOut_FallbackRefusesAnyUnverifiedMember proves an
// ordered fallback list fails closed under opt-out if any member is unverified.
func TestNewPipelineAgent_OptOut_FallbackRefusesAnyUnverifiedMember(t *testing.T) {
	cfg := &config.Config{Agents: []types.AgentName{types.AgentCodex, types.AgentOpenCode}, DisableProjectSettings: true}
	if _, err := newPipelineAgent(context.Background(), cfg, fakeLookPath); err == nil {
		t.Fatal("a fallback list containing an unverified harness must be refused under opt-out")
	}
	cfg = &config.Config{Agents: []types.AgentName{types.AgentCodex, types.AgentClaude}, DisableProjectSettings: true}
	if ag, err := newPipelineAgent(context.Background(), cfg, fakeLookPath); err != nil {
		t.Fatalf("a fallback list of only verified harnesses must pass under opt-out, got: %v", err)
	} else {
		_ = ag.Close()
	}
}
