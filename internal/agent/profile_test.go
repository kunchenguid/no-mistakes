package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// nativeExtraArgs recovers the argv fragment a constructed native adapter will
// splice ahead of its managed flags, so these tests assert what the harness
// actually receives rather than what the mapping table says.
func nativeExtraArgs(t *testing.T, a Agent) []string {
	t.Helper()
	switch v := a.(type) {
	case *claudeAgent:
		return v.extraArgs
	case *codexAgent:
		return v.extraArgs
	case *grokAgent:
		return v.extraArgs
	case *piAgent:
		return v.extraArgs
	case *copilotAgent:
		return v.extraArgs
	case *rovodevAgent:
		return v.extraArgs
	case *opencodeAgent:
		return v.extraArgs
	case *antigravityAgent:
		return v.extraArgs
	default:
		t.Fatalf("agent %T has no extraArgs surface", a)
		return nil
	}
}

// TestNewWithOptions_ProfileReachesEachHarnessArgv is the pipeline half of the
// unified abstraction: one Profile, the right native spelling per harness.
func TestNewWithOptions_ProfileReachesEachHarnessArgv(t *testing.T) {
	profile := agentcfg.Profile{Model: "some-model", Effort: agentcfg.EffortHigh}
	tests := []struct {
		agent types.AgentName
		want  []string
	}{
		{types.AgentClaude, []string{"--model", "some-model", "--effort", "high"}},
		{types.AgentCodex, []string{"-m", "some-model", "-c", `model_reasoning_effort="high"`}},
		{types.AgentGrok, []string{"--model", "some-model", "--reasoning-effort", "high"}},
		{types.AgentCopilot, []string{"--model", "some-model", "--effort", "high"}},
		{types.AgentPi, []string{"--model", "some-model", "--thinking", "high"}},
	}
	for _, tt := range tests {
		t.Run(string(tt.agent), func(t *testing.T) {
			ag, err := NewWithOptions(tt.agent, "bin", nil, Options{Profile: profile})
			if err != nil {
				t.Fatal(err)
			}
			defer ag.Close()
			if got := nativeExtraArgs(t, ag); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("extraArgs = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestNewWithOptions_RawOverrideKeepsWinning is the backwards-compatibility
// contract at the construction funnel: a knob already pinned through
// agent_args_override is never emitted a second time, and the raw argv keeps
// its exact previous shape.
func TestNewWithOptions_RawOverrideKeepsWinning(t *testing.T) {
	raw := []string{"--model", "opus", "--permission-mode", "acceptEdits"}
	ag, err := NewWithOptions(types.AgentClaude, "bin", raw, Options{
		Profile: agentcfg.Profile{Model: "sonnet", Effort: agentcfg.EffortLow},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	want := []string{"--model", "opus", "--permission-mode", "acceptEdits", "--effort", "low"}
	if got := nativeExtraArgs(t, ag); !reflect.DeepEqual(got, want) {
		t.Fatalf("extraArgs = %v, want %v", got, want)
	}
	ca := ag.(*claudeAgent)
	built := ca.buildArgs(nil, "")
	if strings.Count(strings.Join(built, " "), "--model") != 1 {
		t.Fatalf("claude received --model more than once: %v", built)
	}
}

// TestNewWithOptions_ZeroProfileLeavesArgvUntouched is the no-op guarantee for
// every configuration written before the common layer existed.
func TestNewWithOptions_ZeroProfileLeavesArgvUntouched(t *testing.T) {
	raw := []string{"-c", `service_tier="priority"`}
	ag, err := NewWithOptions(types.AgentCodex, "bin", raw, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	if got := nativeExtraArgs(t, ag); !reflect.DeepEqual(got, raw) {
		t.Fatalf("extraArgs = %v, want the raw args unchanged %v", got, raw)
	}
}

func TestNewWithOptions_DoesNotMutateCallerArgs(t *testing.T) {
	raw := make([]string, 2, 8)
	raw[0], raw[1] = "--permission-mode", "acceptEdits"
	before := append([]string(nil), raw...)
	if _, err := NewWithOptions(types.AgentClaude, "bin", raw, Options{
		Profile: agentcfg.Profile{Model: "sonnet"},
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw, before) {
		t.Fatalf("caller args mutated: %v, want %v", raw, before)
	}
}

// TestNewWithOptions_RefusesUnmappableKnob keeps the construction funnel
// fail-closed, so a programmatic caller that skipped config validation still
// cannot launch a harness that will ignore the request.
func TestNewWithOptions_RefusesUnmappableKnob(t *testing.T) {
	tests := []struct {
		agent   types.AgentName
		profile agentcfg.Profile
	}{
		{types.AgentRovoDev, agentcfg.Profile{Model: "x"}},
		{types.AgentAntigravity, agentcfg.Profile{Effort: agentcfg.EffortHigh}},
		{types.AgentCursor, agentcfg.Profile{Effort: agentcfg.EffortHigh}},
		{types.AgentOpenCode, agentcfg.Profile{Model: "gpt-5"}},
	}
	for _, tt := range tests {
		ag, err := NewWithOptions(tt.agent, "bin", nil, Options{Profile: tt.profile})
		if err == nil {
			ag.Close()
			t.Fatalf("NewWithOptions(%s, %+v) succeeded, want refusal", tt.agent, tt.profile)
		}
	}
}

// TestACPModelIsPinnedOnTheAcpxCommand covers the closed ACP gap end to end:
// the model reaches acpx's own flag, positioned among acpx options rather than
// after the target or the exec subcommand.
func TestACPModelIsPinnedOnTheAcpxCommand(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentCursor, "acp:custom"} {
		ag, err := NewWithOptions(name, "acpx", nil, Options{
			Profile: agentcfg.Profile{Model: "gpt-5"},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer ag.Close()
		args := ag.(*acpxAgent).buildArgs(RunOpts{CWD: "/w"})
		modelIdx, execIdx := -1, -1
		for i, arg := range args {
			switch arg {
			case "--model":
				modelIdx = i
			case "exec":
				execIdx = i
			}
		}
		if modelIdx < 0 || args[modelIdx+1] != "gpt-5" {
			t.Fatalf("%s argv missing --model gpt-5: %v", name, args)
		}
		if execIdx < 0 || modelIdx > execIdx {
			t.Fatalf("%s placed --model outside acpx's own options: %v", name, args)
		}
	}
}

func TestACPWithoutModelKeepsItsPreviousArgv(t *testing.T) {
	plain, err := NewWithOptions(types.AgentCursor, "acpx", nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	for _, arg := range plain.(*acpxAgent).buildArgs(RunOpts{CWD: "/w"}) {
		if arg == "--model" {
			t.Fatal("acpx received --model with no model pinned")
		}
	}
}

// TestOpenCodeProfileRidesTheMessageBody covers the one harness whose launch
// command cannot carry either knob: `opencode serve` exits with usage on an
// unknown flag, so an argv pin would take the server down instead of tuning it.
func TestOpenCodeProfileRidesTheMessageBody(t *testing.T) {
	ag, err := NewWithOptions(types.AgentOpenCode, "opencode", nil, Options{
		Profile: agentcfg.Profile{Model: "openai/gpt-5", Effort: agentcfg.EffortHigh},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	oc := ag.(*opencodeAgent)
	if len(oc.extraArgs) != 0 {
		t.Fatalf("opencode serve argv gained %v; the server rejects model flags", oc.extraArgs)
	}
	body := oc.messageBody("prompt", nil)
	model, ok := body["model"].(map[string]string)
	if !ok || model["providerID"] != "openai" || model["modelID"] != "gpt-5" {
		t.Fatalf("message body model = %#v", body["model"])
	}
	if body["variant"] != "high" {
		t.Fatalf("message body variant = %#v, want high", body["variant"])
	}
}

func TestOpenCodeWithoutProfileSendsNoModelOrVariant(t *testing.T) {
	ag, err := NewWithOptions(types.AgentOpenCode, "opencode", nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	body := ag.(*opencodeAgent).messageBody("prompt", json.RawMessage(`{"type":"object"}`))
	if _, ok := body["model"]; ok {
		t.Fatalf("message body pinned a model with no profile: %#v", body)
	}
	if _, ok := body["variant"]; ok {
		t.Fatalf("message body pinned a variant with no profile: %#v", body)
	}
	if _, ok := body["format"]; ok {
		t.Fatalf("message body used root structured-output format: %#v", body)
	}
	info, ok := body["info"].(map[string]any)
	if !ok {
		t.Fatalf("message body omitted structured-output info: %#v", body)
	}
	if _, ok := info["format"]; !ok {
		t.Fatalf("message body omitted structured-output info.format: %#v", body)
	}
}
