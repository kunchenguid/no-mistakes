package agent

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// optOutAgent builds an adapter with the trusted opt-out ON, mirroring how the
// daemon constructs gate agents when disable_project_settings=true.
func optOutAgent(t *testing.T, name types.AgentName, extraArgs []string) Agent {
	t.Helper()
	a, err := NewWithOptions(name, string(name), extraArgs, Options{DisableProjectSettings: true})
	if err != nil {
		t.Fatalf("NewWithOptions(%s): %v", name, err)
	}
	return a
}

// TestNeutralizesGateInstructions_OnlyVerifiedHarnessesUnderOptOut is the core
// fail-closed contract: under the opt-out, only codex and claude (whose
// suppression knobs are empirically verified) neutralize the target repo's
// project agent settings/instructions; every other harness reports false and is
// refused rather than launched with project instructions loaded.
func TestNeutralizesGateInstructions_OnlyVerifiedHarnessesUnderOptOut(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentCodex, types.AgentClaude} {
		if !NeutralizesGateInstructions(optOutAgent(t, name, nil)) {
			t.Errorf("%s must neutralize under the opt-out with its default knob", name)
		}
	}
	unverified := []types.AgentName{types.AgentOpenCode, types.AgentPi, types.AgentCopilot, types.AgentRovoDev}
	for _, name := range unverified {
		if NeutralizesGateInstructions(optOutAgent(t, name, nil)) {
			t.Errorf("%s has no verified knob; must NOT report neutralized", name)
		}
	}
	acp, err := NewWithOptions(types.AgentName("acp:some-target"), "acpx", nil, Options{DisableProjectSettings: true})
	if err != nil {
		t.Fatalf("acp NewWithOptions: %v", err)
	}
	if NeutralizesGateInstructions(acp) {
		t.Error("acp adapter must NOT report neutralized")
	}
	if NeutralizesGateInstructions(NewNoop()) {
		t.Error("noop agent must NOT report neutralized")
	}
	if NeutralizesGateInstructions(nil) {
		t.Error("nil agent must NOT report neutralized")
	}
}

// TestNeutralizesGateInstructions_FalseWithoutOptOut proves codex/claude do NOT
// claim neutralization when the repo did not opt out - the gate only consults
// this under the opt-out, but the value must be honest.
func TestNeutralizesGateInstructions_FalseWithoutOptOut(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentCodex, types.AgentClaude} {
		a, err := NewWithOptions(name, string(name), nil, Options{}) // no opt-out
		if err != nil {
			t.Fatalf("NewWithOptions(%s): %v", name, err)
		}
		if NeutralizesGateInstructions(a) {
			t.Errorf("%s must not report neutralized when the repo did not opt out", name)
		}
	}
}

// TestEnsureGateNeutralized_RefusesUnsupportedUnderOptOut proves the gate fails
// closed for an unsupported harness with a clear error, and admits codex/claude.
func TestEnsureGateNeutralized_RefusesUnsupportedUnderOptOut(t *testing.T) {
	if err := EnsureGateNeutralized(optOutAgent(t, types.AgentCodex, nil)); err != nil {
		t.Errorf("codex must pass the gate under opt-out: %v", err)
	}
	if err := EnsureGateNeutralized(optOutAgent(t, types.AgentClaude, nil)); err != nil {
		t.Errorf("claude must pass the gate under opt-out: %v", err)
	}
	err := EnsureGateNeutralized(optOutAgent(t, types.AgentOpenCode, nil))
	if err == nil {
		t.Fatal("opencode must be refused by the gate under opt-out")
	}
	if !strings.Contains(err.Error(), "does not neutralize") || !strings.Contains(err.Error(), "opencode") {
		t.Errorf("refusal error should name the harness and reason, got: %v", err)
	}
	if err := EnsureGateNeutralized(nil); err == nil {
		t.Error("a nil agent must be refused")
	}
}

// TestNeutralizesGateInstructions_ThroughProductionWrapping mirrors how the
// daemon builds the run agent (WithSteering per adapter, then NewFallback) and
// proves the capability propagates through both wrappers and fails closed if ANY
// fallback member is unverified.
func TestNeutralizesGateInstructions_ThroughProductionWrapping(t *testing.T) {
	if !NeutralizesGateInstructions(WithSteering(optOutAgent(t, types.AgentCodex, nil))) {
		t.Error("WithSteering(codex) must remain neutralized under opt-out")
	}
	if NeutralizesGateInstructions(WithSteering(optOutAgent(t, types.AgentOpenCode, nil))) {
		t.Error("WithSteering(opencode) must remain non-neutralized")
	}
	allVerified := NewFallback([]Agent{
		WithSteering(optOutAgent(t, types.AgentCodex, nil)),
		WithSteering(optOutAgent(t, types.AgentClaude, nil)),
	})
	if err := EnsureGateNeutralized(allVerified); err != nil {
		t.Errorf("fallback [codex, claude] must pass under opt-out: %v", err)
	}
	oneUnverified := NewFallback([]Agent{
		WithSteering(optOutAgent(t, types.AgentCodex, nil)),
		WithSteering(optOutAgent(t, types.AgentOpenCode, nil)),
	})
	if err := EnsureGateNeutralized(oneUnverified); err == nil {
		t.Error("fallback [codex, opencode] must be refused under opt-out")
	}
}

// TestNeutralizesGateInstructions_HonestOnEffectiveOverride proves the capability
// is honest about the EFFECTIVE knob value: a preserving operator override is
// admitted; a defeating one fails closed - even for codex/claude.
func TestNeutralizesGateInstructions_HonestOnEffectiveOverride(t *testing.T) {
	// codex: project_doc_max_bytes=0 preserves suppression -> admitted.
	if !NeutralizesGateInstructions(optOutAgent(t, types.AgentCodex, []string{"-c", "project_doc_max_bytes=0"})) {
		t.Error("codex with an explicit project_doc_max_bytes=0 must stay neutralized")
	}
	// codex: project_doc_max_bytes>0 re-enables the doc -> fails closed.
	if NeutralizesGateInstructions(optOutAgent(t, types.AgentCodex, []string{"-c", "project_doc_max_bytes=4096"})) {
		t.Error("codex with project_doc_max_bytes=4096 must fail closed")
	}
	if err := EnsureGateNeutralized(optOutAgent(t, types.AgentCodex, []string{"-c", "project_doc_max_bytes=4096"})); err == nil {
		t.Error("codex with the knob defeated must be refused by the gate")
	}
	// claude: --setting-sources user preserves suppression -> admitted.
	if !NeutralizesGateInstructions(optOutAgent(t, types.AgentClaude, []string{"--setting-sources", "user"})) {
		t.Error("claude with an explicit --setting-sources user must stay neutralized")
	}
	// claude: --setting-sources re-adding project -> fails closed.
	if NeutralizesGateInstructions(optOutAgent(t, types.AgentClaude, []string{"--setting-sources", "user,project"})) {
		t.Error("claude with --setting-sources user,project must fail closed")
	}
	if err := EnsureGateNeutralized(optOutAgent(t, types.AgentClaude, []string{"--setting-sources", "user,local"})); err == nil {
		t.Error("claude re-adding local must be refused by the gate")
	}
}
