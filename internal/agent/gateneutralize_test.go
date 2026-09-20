package agent

import (
	"os"
	"path/filepath"
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
// fail-closed contract: under the opt-out, only codex, claude, and pi (whose
// suppression knobs are empirically verified) neutralize the target repo's
// project agent settings/instructions; every other harness reports false and is
// refused rather than launched with project instructions loaded.
func TestNeutralizesGateInstructions_OnlyVerifiedHarnessesUnderOptOut(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentCodex, types.AgentClaude, types.AgentPi} {
		if !NeutralizesGateInstructions(optOutAgent(t, name, nil)) {
			t.Errorf("%s must neutralize under the opt-out with its default knob", name)
		}
	}
	unverified := []types.AgentName{types.AgentGrok, types.AgentOpenCode, types.AgentCopilot, types.AgentRovoDev}
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

// TestNeutralizesGateInstructions_FalseWithoutOptOut proves codex, claude, and pi
// do NOT claim neutralization when the repo did not opt out - the gate only consults
// this under the opt-out, but the value must be honest.
func TestNeutralizesGateInstructions_FalseWithoutOptOut(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentCodex, types.AgentClaude, types.AgentPi, types.AgentGrok} {
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
// closed for an unsupported harness and admits codex, claude, and pi.
func TestEnsureGateNeutralized_RefusesUnsupportedUnderOptOut(t *testing.T) {
	if err := EnsureGateNeutralized(optOutAgent(t, types.AgentCodex, nil)); err != nil {
		t.Errorf("codex must pass the gate under opt-out: %v", err)
	}
	if err := EnsureGateNeutralized(optOutAgent(t, types.AgentClaude, nil)); err != nil {
		t.Errorf("claude must pass the gate under opt-out: %v", err)
	}
	if err := EnsureGateNeutralized(optOutAgent(t, types.AgentPi, nil)); err != nil {
		t.Errorf("pi must pass the gate under opt-out: %v", err)
	}
	if err := EnsureGateNeutralized(optOutAgent(t, types.AgentGrok, nil)); err == nil {
		t.Error("grok must remain refused until project-setting isolation is empirically verified")
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
	if !NeutralizesGateInstructions(WithSteering(optOutAgent(t, types.AgentCodex, nil), "/evidence")) {
		t.Error("WithSteering(codex) must remain neutralized under opt-out")
	}
	if NeutralizesGateInstructions(WithSteering(optOutAgent(t, types.AgentOpenCode, nil), "/evidence")) {
		t.Error("WithSteering(opencode) must remain non-neutralized")
	}
	allVerified := NewFallback([]Agent{
		WithSteering(optOutAgent(t, types.AgentCodex, nil), "/evidence"),
		WithSteering(optOutAgent(t, types.AgentClaude, nil), "/evidence"),
	})
	if err := EnsureGateNeutralized(allVerified); err != nil {
		t.Errorf("fallback [codex, claude] must pass under opt-out: %v", err)
	}
	oneUnverified := NewFallback([]Agent{
		WithSteering(optOutAgent(t, types.AgentCodex, nil), "/evidence"),
		WithSteering(optOutAgent(t, types.AgentOpenCode, nil), "/evidence"),
	})
	if err := EnsureGateNeutralized(oneUnverified); err == nil {
		t.Error("fallback [codex, opencode] must be refused under opt-out")
	}
}

// TestNeutralizesGateInstructions_HonestOnEffectiveOverride proves the capability
// is honest about the EFFECTIVE knob value: preserving operator overrides are
// admitted, while available defeating overrides fail closed.
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
	// pi: explicit -nc/--no-context-files preserves suppression -> admitted.
	if !NeutralizesGateInstructions(optOutAgent(t, types.AgentPi, []string{"--no-context-files"})) {
		t.Error("pi with an explicit --no-context-files must stay neutralized")
	}
	if !NeutralizesGateInstructions(optOutAgent(t, types.AgentPi, []string{"-nc"})) {
		t.Error("pi with an explicit -nc must stay neutralized")
	}
}

// acpOptOutAgent builds an acpx adapter with the trusted opt-out ON. It passes
// "acpx" as the binary (the real acpx shim name) rather than the agent name so
// the target and raw-command resolution match production.
func acpOptOutAgent(t *testing.T, name types.AgentName, overrides map[string]string) *acpxAgent {
	t.Helper()
	a, err := NewWithOptions(name, "acpx", nil, Options{
		DisableProjectSettings: true,
		ACPRegistryOverrides:   overrides,
	})
	if err != nil {
		t.Fatalf("NewWithOptions(%s): %v", name, err)
	}
	acpx, ok := a.(*acpxAgent)
	if !ok {
		t.Fatalf("agent type = %T, want *acpxAgent", a)
	}
	return acpx
}

// TestNeutralizesGateInstructions_OMPTargetUnderOptOut is the omp gate-agent
// admission contract: acp:omp neutralizes (and the gate admits it) only under
// the opt-out and only on its default launch. A non-omp acp target, the same
// target without the opt-out, and an operator raw-command override all fail
// closed - a custom command is opaque, so we cannot prove it applies the
// suppression overlay and flags.
func TestNeutralizesGateInstructions_OMPTargetUnderOptOut(t *testing.T) {
	omp := acpOptOutAgent(t, types.AgentName("acp:omp"), nil)
	if !NeutralizesGateInstructions(omp) {
		t.Fatal("acp:omp must neutralize under the opt-out on its default launch")
	}
	if err := EnsureGateNeutralized(omp); err != nil {
		t.Fatalf("acp:omp must pass the gate under the opt-out: %v", err)
	}

	// Without the opt-out the value must be honest: false.
	noOptOut, err := NewWithOptions(types.AgentName("acp:omp"), "acpx", nil, Options{})
	if err != nil {
		t.Fatalf("NewWithOptions(acp:omp): %v", err)
	}
	if NeutralizesGateInstructions(noOptOut) {
		t.Error("acp:omp must not report neutralized when the repo did not opt out")
	}

	// A different acp target is not verified and must fail closed.
	other := acpOptOutAgent(t, types.AgentName("acp:gemini"), nil)
	if NeutralizesGateInstructions(other) {
		t.Error("acp:gemini has no verified knob; must NOT report neutralized")
	}
	if err := EnsureGateNeutralized(other); err == nil {
		t.Error("acp:gemini must be refused by the gate under the opt-out")
	}

	// An operator raw-command override for omp is opaque -> fail closed.
	overridden := acpOptOutAgent(t, types.AgentName("acp:omp"), map[string]string{"omp": "omp acp --profile custom"})
	if NeutralizesGateInstructions(overridden) {
		t.Error("acp:omp with an operator raw-command override must fail closed")
	}
	if err := EnsureGateNeutralized(overridden); err == nil {
		t.Error("acp:omp with an override must be refused by the gate under the opt-out")
	}
}

// TestOMPGateNeutralization_AppliesSuppressionOverlayAndFlags proves the wiring,
// not just the capability bool: a neutralized acp:omp launch replaces the
// built-in omp target with an `omp acp` command that carries the context-file
// suppression overlay (via --config) and the rule/skill/extension flags, and the
// overlay it writes disables every omp context-file discovery provider. Close
// removes the overlay file.
func TestOMPGateNeutralization_AppliesSuppressionOverlayAndFlags(t *testing.T) {
	omp := acpOptOutAgent(t, types.AgentName("acp:omp"), nil)

	rawCommand, err := omp.resolveRawCommand()
	if err != nil {
		t.Fatalf("resolveRawCommand: %v", err)
	}
	overlayPath := omp.overlayPath
	if overlayPath == "" {
		t.Fatal("neutralized omp launch must write a suppression overlay")
	}
	// The overlay path MUST be absolute: acpx composes it into the --agent
	// command while omp resolves --config relative to the gate's CWD (the target
	// checkout). A relative path (from a relative TMPDIR) would resolve against
	// the wrong directory and miss - and since omp fails closed on a missing
	// overlay, that miss would abort every gate run. An absolute path is what
	// makes the intended overlay load regardless of the launch cwd.
	if !filepath.IsAbs(overlayPath) {
		t.Fatalf("overlay path must be absolute, got %q", overlayPath)
	}

	// The launch is `omp acp --config <overlay>` plus the suppression flags, and
	// deliberately does NOT fall through to acpx's built-in omp target.
	wantPrefix := "omp acp --config " + overlayPath
	if !strings.HasPrefix(rawCommand, wantPrefix) {
		t.Fatalf("raw command = %q, want prefix %q", rawCommand, wantPrefix)
	}
	for _, flag := range []string{"--no-rules", "--no-skills", "--no-extensions"} {
		if !strings.Contains(rawCommand, " "+flag) {
			t.Errorf("raw command = %q, missing suppression flag %q", rawCommand, flag)
		}
	}

	args := omp.buildArgs(rawCommand, RunOpts{CWD: "/repo"})
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "--agent\x00"+rawCommand) {
		t.Fatalf("args = %q, want the neutralized command behind --agent", args)
	}
	if strings.Contains(joined, "\x00omp\x00") {
		t.Fatalf("args = %q, must not also append the bare omp target subcommand", args)
	}

	overlay, err := os.ReadFile(overlayPath)
	if err != nil {
		t.Fatalf("read overlay: %v", err)
	}
	body := string(overlay)
	if !strings.Contains(body, "disabledProviders:") {
		t.Fatalf("overlay does not disable providers:\n%s", body)
	}
	for _, provider := range []string{"native", "claude", "codex", "gemini", "opencode", "github", "agents", "agents-md", "claude-md"} {
		if !strings.Contains(body, "- "+provider+"\n") {
			t.Errorf("overlay does not disable context-file provider %q:\n%s", provider, body)
		}
	}
	if !strings.Contains(body, "memory:") || !strings.Contains(body, "backend: off") {
		t.Errorf("overlay does not disable mnemopi memory:\n%s", body)
	}

	if err := omp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(overlayPath); !os.IsNotExist(err) {
		t.Errorf("Close must remove the overlay file, stat err = %v", err)
	}
}

// TestOverlayPathShellSafe pins the cross-platform path rule that keeps
// windows-core green: a Windows temp path (backslashes, drive colon) composes
// safely into acpx's --agent command, while whitespace and quotes are unsafe on
// every OS and a backslash is anomalous - and refused - off Windows. goos is a
// parameter so both branches are proven from any host.
func TestOverlayPathShellSafe(t *testing.T) {
	cases := []struct {
		name string
		path string
		goos string
		want bool
	}{
		{"windows temp path is safe", `C:\Users\runneradmin\AppData\Local\Temp\nm-omp-gate-1.yml`, "windows", true},
		{"windows path with space is unsafe", `C:\Users\John Doe\Temp\nm-omp-gate-1.yml`, "windows", false},
		{"posix temp path is safe", "/tmp/nm-omp-gate-1.yml", "linux", true},
		{"posix relative-resolved path is safe", "/var/folders/xy/T/nm-omp-gate-1.yml", "darwin", true},
		{"backslash is unsafe off windows", `/tmp/a\b/nm-omp-gate-1.yml`, "linux", false},
		{"posix path with space is unsafe", "/tmp/a b/nm-omp-gate-1.yml", "linux", false},
		{"quote is unsafe on windows too", `C:\Temp\a"b\nm-omp-gate-1.yml`, "windows", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := overlayPathShellSafe(tc.path, tc.goos); got != tc.want {
				t.Errorf("overlayPathShellSafe(%q, %q) = %v, want %v", tc.path, tc.goos, got, tc.want)
			}
		})
	}
}
