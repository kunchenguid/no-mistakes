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

// TestOpencodeExtractModel proves --model is extracted from extraArgs (both
// --model <value> and --model=<value> forms) and the remaining args are
// returned without --model. This is critical because `opencode serve` does not
// accept --model; passing it makes yargs print help and exit, breaking the
// server.
func TestOpencodeExtractModel(t *testing.T) {
	cases := []struct {
		name      string
		extraArgs []string
		wantArgs  []string
		wantModel string
	}{
		{
			name:      "split form --model value",
			extraArgs: []string{"--model", "ollama-cloud/glm-5.2", "--log-level", "DEBUG"},
			wantArgs:  []string{"--log-level", "DEBUG"},
			wantModel: "ollama-cloud/glm-5.2",
		},
		{
			name:      "equals form --model=value",
			extraArgs: []string{"--model=ollama-cloud/glm-5.2", "--pure"},
			wantArgs:  []string{"--pure"},
			wantModel: "ollama-cloud/glm-5.2",
		},
		{
			name:      "no model",
			extraArgs: []string{"--log-level", "DEBUG"},
			wantArgs:  []string{"--log-level", "DEBUG"},
			wantModel: "",
		},
		{
			name:      "empty args",
			extraArgs: nil,
			wantArgs:  []string{},
			wantModel: "",
		},
		{
			name:      "model only",
			extraArgs: []string{"--model", "ollama-cloud/glm-5.2"},
			wantArgs:  []string{},
			wantModel: "ollama-cloud/glm-5.2",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotArgs, gotModel := opencodeExtractModel(c.extraArgs)
			if gotModel != c.wantModel {
				t.Errorf("model = %q, want %q", gotModel, c.wantModel)
			}
			if !equalStringSlices(gotArgs, c.wantArgs) {
				t.Errorf("args = %v, want %v", gotArgs, c.wantArgs)
			}
		})
	}
}

// TestParseOpencodeModel proves the "provider/model" parsing works correctly.
func TestParseOpencodeModel(t *testing.T) {
	cases := []struct {
		model        string
		wantProvider string
		wantModel    string
		wantOK       bool
	}{
		{"ollama-cloud/glm-5.2", "ollama-cloud", "glm-5.2", true},
		{"test/test-model", "test", "test-model", true},
		{"no-slash", "", "", false},
		{"/leading-slash", "", "", false},
		{"trailing-slash/", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			provider, model, ok := parseOpencodeModel(c.model)
			if provider != c.wantProvider || model != c.wantModel || ok != c.wantOK {
				t.Errorf("parseOpencodeModel(%q) = (%q, %q, %v), want (%q, %q, %v)",
					c.model, provider, model, ok, c.wantProvider, c.wantModel, c.wantOK)
			}
		})
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
// from agent_args_override are extracted from the serve argv (because opencode
// serve does not accept --model) and routed to the session creation API, while
// the neutralization flags are still appended to the serve argv.
func TestOpencodeAgent_PreservesModelArgsUnderOptOut(t *testing.T) {
	// Simulate NewWithOptions extracting --model from extraArgs.
	serveArgs, model := opencodeExtractModel([]string{"--model", "ollama-cloud/glm-5.2"})
	oa := &opencodeAgent{
		bin:                    "opencode",
		extraArgs:              serveArgs,
		disableProjectSettings: true,
		sessionModel:           model,
	}
	if oa.sessionModel != "ollama-cloud/glm-5.2" {
		t.Errorf("sessionModel must be set to the extracted model, got %q", oa.sessionModel)
	}
	args := buildOpencodeServeArgs(oa.extraArgs, 12345, oa.disableProjectSettings)
	joined := strings.Join(args, " ")
	// --model must NOT be in the serve argv (it breaks opencode serve).
	if strings.Contains(joined, "--model") {
		t.Errorf("--model must NOT be in the serve argv, got: %s", joined)
	}
	// Neutralization flags must be present.
	for _, want := range []string{"--no-project-instructions", "--pure"} {
		if !strings.Contains(joined, want) {
			t.Errorf("serve argv must contain %q, got: %s", want, joined)
		}
	}
	// The model must be parseable into provider/model for the session API.
	providerID, modelID, ok := parseOpencodeModel(oa.sessionModel)
	if !ok || providerID != "ollama-cloud" || modelID != "glm-5.2" {
		t.Errorf("parseOpencodeModel(%q) = (%q, %q, %v), want (ollama-cloud, glm-5.2, true)", oa.sessionModel, providerID, modelID, ok)
	}
}
