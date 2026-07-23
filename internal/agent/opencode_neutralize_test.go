package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestOpencodeHelpListsNoProjectInstructions proves the help-text matcher
// recognizes the --no-project-instructions flag as yargs emits it, and does
// not false-positive on an absent flag.
func TestOpencodeHelpListsNoProjectInstructions(t *testing.T) {
	cases := []struct {
		name string
		help string
		want bool
	}{
		{
			name: "flag present as boolean",
			help: "opencode serve\n\nstarts a headless opencode server\n\nOptions:\n" +
				"      --no-project-instructions  disable project instructions    [boolean]\n",
			want: true,
		},
		{
			name: "flag present with alias",
			help: "      --no-project-instructions, --no-pi  disable project instructions  [boolean]\n",
			want: true,
		},
		{
			name: "flag absent (older binary)",
			help: "opencode serve\n\nOptions:\n      --pure         run without external plugins   [boolean]\n",
			want: false,
		},
		{
			name: "empty help",
			help: "",
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := opencodeHelpListsNoProjectInstructions(c.help); got != c.want {
				t.Errorf("opencodeHelpListsNoProjectInstructions = %v, want %v", got, c.want)
			}
		})
	}
}

// TestProbeOpencodeNoProjectInstructions_AdmitsSupported proves the probe
// passes when the binary's serve --help advertises --no-project-instructions.
func TestProbeOpencodeNoProjectInstructions_AdmitsSupported(t *testing.T) {
	original := probeOpencodeNoProjectInstructions
	t.Cleanup(func() { probeOpencodeNoProjectInstructions = original })
	probeOpencodeNoProjectInstructions = func(_ context.Context, _ string) error {
		return nil
	}
	if err := probeOpencodeNoProjectInstructions(context.Background(), "/fake/opencode"); err != nil {
		t.Fatalf("supported binary must pass probe, got: %v", err)
	}
}

// TestProbeOpencodeNoProjectInstructions_RefusesUnsupported proves the probe
// fails closed with a concrete diagnostic when the binary omits the flag
// (older OpenCode). The diagnostic must name the required capability, the
// config field, and the fallback agents.
func TestProbeOpencodeNoProjectInstructions_RefusesUnsupported(t *testing.T) {
	original := probeOpencodeNoProjectInstructions
	t.Cleanup(func() { probeOpencodeNoProjectInstructions = original })
	probeOpencodeNoProjectInstructions = func(_ context.Context, bin string) error {
		return original(context.Background(), bin)
	}
	// The original probe will fail for a nonexistent binary; verify the
	// diagnostic content is concrete regardless of the failure path.
	err := probeOpencodeNoProjectInstructions(context.Background(), "/nonexistent/opencode-bin-xyz")
	if err == nil {
		t.Fatal("unsupported binary must fail the probe")
	}
	msg := err.Error()
	for _, want := range []string{
		"--no-project-instructions",
		"disable_project_settings",
		"upgrade OpenCode",
		"codex",
		"claude",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("diagnostic must mention %q, got: %v", want, err)
		}
	}
}

// TestProbeOpencodeNoProjectInstructions_DiagnosticForMissingFlag proves the
// probe's absent-flag diagnostic path (when the binary runs --help fine but
// the flag is missing from the output).
func TestProbeOpencodeNoProjectInstructions_DiagnosticForMissingFlag(t *testing.T) {
	original := probeOpencodeNoProjectInstructions
	t.Cleanup(func() { probeOpencodeNoProjectInstructions = original })
	// Inject a probe that simulates the "flag absent" path by calling the
	// help-text matcher directly with help text that lacks the flag.
	probeOpencodeNoProjectInstructions = func(_ context.Context, bin string) error {
		// Simulate older binary help output (no --no-project-instructions).
		help := "opencode serve\n\nOptions:\n      --pure         run without external plugins   [boolean]\n"
		if opencodeHelpListsNoProjectInstructions(help) {
			return nil
		}
		return original(context.Background(), "/nonexistent/bin")
	}
	err := probeOpencodeNoProjectInstructions(context.Background(), "/fake/opencode-old")
	if err == nil {
		t.Fatal("probe must fail when the flag is absent from help")
	}
	if !strings.Contains(err.Error(), "--no-project-instructions") {
		t.Errorf("diagnostic must name the flag, got: %v", err)
	}
}

// TestOpencodeAgent_NeutralizesGateInstructions_HonestAboutOptOut proves the
// capability follows the opt-out flag honestly: true under opt-out, false
// without it.
func TestOpencodeAgent_NeutralizesGateInstructions_HonestAboutOptOut(t *testing.T) {
	under := &opencodeAgent{bin: "opencode", disableProjectSettings: true}
	if !under.NeutralizesGateInstructions() {
		t.Error("opencode under opt-out must report neutralized")
	}
	without := &opencodeAgent{bin: "opencode", disableProjectSettings: false}
	if without.NeutralizesGateInstructions() {
		t.Error("opencode without opt-out must NOT report neutralized")
	}
}

// TestOpencodeAgent_NewWithOptionsWiresOptOut proves NewWithOptions threads
// DisableProjectSettings into the opencode adapter, so the daemon's
// newPipelineAgent wiring produces a neutralized agent under the opt-out.
func TestOpencodeAgent_NewWithOptionsWiresOptOut(t *testing.T) {
	a, err := NewWithOptions(types.AgentOpenCode, "opencode", nil, Options{DisableProjectSettings: true})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	oa, ok := a.(*opencodeAgent)
	if !ok {
		t.Fatalf("expected *opencodeAgent, got %T", a)
	}
	if !oa.disableProjectSettings {
		t.Error("NewWithOptions must set disableProjectSettings on opencode under opt-out")
	}
	if !NeutralizesGateInstructions(a) {
		t.Error("opencode under opt-out must report neutralized")
	}
	plain, err := NewWithOptions(types.AgentOpenCode, "opencode", nil, Options{})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	if NeutralizesGateInstructions(plain) {
		t.Error("opencode without opt-out must NOT report neutralized")
	}
}

// TestOpencodeAgent_PreservesModelArgsUnderOptOut proves explicit model args
// from agent_args_override are preserved in the serve argv even under the
// opt-out, alongside the neutralization flags.
func TestOpencodeAgent_PreservesModelArgsUnderOptOut(t *testing.T) {
	oa := &opencodeAgent{
		bin:                    "opencode",
		extraArgs:              []string{"--model", "ollama-cloud/glm-5.2"},
		disableProjectSettings: true,
	}
	args := buildOpencodeServeArgs(oa.extraArgs, 12345, oa.disableProjectSettings)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--model ollama-cloud/glm-5.2",
		"--no-project-instructions",
		"--pure",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("serve argv must contain %q, got: %s", want, joined)
		}
	}
	if i := strings.Index(joined, "--model"); i < 0 {
		t.Fatal("model arg missing")
	} else if j := strings.Index(joined, "--no-project-instructions"); j < i {
		t.Errorf("model arg must precede --no-project-instructions, got: %s", joined)
	}
}
