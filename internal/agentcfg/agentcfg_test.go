package agentcfg

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestNativeArgsMapsEachHarnessToItsOwnSpelling pins the whole point of the
// package: one common Profile, one native argv fragment per harness.
func TestNativeArgsMapsEachHarnessToItsOwnSpelling(t *testing.T) {
	profile := Profile{Model: "some-model", Effort: EffortLow}
	tests := []struct {
		agent types.AgentName
		want  []string
	}{
		{types.AgentClaude, []string{"--model", "some-model", "--effort", "low"}},
		{types.AgentCodex, []string{"-m", "some-model", "-c", `model_reasoning_effort="low"`}},
		{types.AgentGrok, []string{"--model", "some-model", "--reasoning-effort", "low"}},
		{types.AgentCopilot, []string{"--model", "some-model", "--effort", "low"}},
		{types.AgentPi, []string{"--model", "some-model", "--thinking", "low"}},
	}
	for _, tt := range tests {
		t.Run(string(tt.agent), func(t *testing.T) {
			got := NativeArgs(tt.agent, profile, nil)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("NativeArgs(%s) = %v, want %v", tt.agent, got, tt.want)
			}
		})
	}
}

// TestNativeArgsEmitsNothingForNonArgsHarnesses keeps a knob that rides another
// mechanism (or none at all) out of argv, where it would either be rejected by
// the CLI or silently ignored.
func TestNativeArgsEmitsNothingForNonArgsHarnesses(t *testing.T) {
	profile := Profile{Model: "openai/gpt-5", Effort: EffortHigh}
	for _, name := range []types.AgentName{types.AgentOpenCode, types.AgentRovoDev, types.AgentAntigravity} {
		if got := NativeArgs(name, profile, nil); got != nil {
			t.Errorf("NativeArgs(%s) = %v, want nil", name, got)
		}
	}
}

func TestNativeArgsZeroProfileIsAlwaysEmpty(t *testing.T) {
	for _, name := range append(Agents(), types.AgentCursor, "acp:gemini") {
		if got := NativeArgs(name, Profile{}, []string{"--model", "pinned"}); got != nil {
			t.Errorf("NativeArgs(%s, zero) = %v, want nil", name, got)
		}
	}
}

// TestNativeArgsDefersToRawOverride is the backwards-compatibility contract:
// an operator who already pinned a knob natively keeps that exact argv, and the
// harness is never handed the same knob twice.
func TestNativeArgsDefersToRawOverride(t *testing.T) {
	tests := []struct {
		name    string
		agent   types.AgentName
		profile Profile
		raw     []string
		want    []string
	}{
		{
			name:    "claude model pinned by flag",
			agent:   types.AgentClaude,
			profile: Profile{Model: "sonnet", Effort: EffortHigh},
			raw:     []string{"--model", "opus"},
			want:    []string{"--effort", "high"},
		},
		{
			name:    "claude model pinned by equals form",
			agent:   types.AgentClaude,
			profile: Profile{Model: "sonnet"},
			raw:     []string{"--model=opus"},
			want:    nil,
		},
		{
			name:    "claude effort pinned",
			agent:   types.AgentClaude,
			profile: Profile{Model: "sonnet", Effort: EffortHigh},
			raw:     []string{"--effort", "max"},
			want:    []string{"--model", "sonnet"},
		},
		{
			name:    "codex model pinned by short flag",
			agent:   types.AgentCodex,
			profile: Profile{Model: "gpt-5.4", Effort: EffortLow},
			raw:     []string{"-m", "o3"},
			want:    []string{"-c", `model_reasoning_effort="low"`},
		},
		{
			name:    "codex model pinned by config override",
			agent:   types.AgentCodex,
			profile: Profile{Model: "gpt-5.4"},
			raw:     []string{"-c", `model="o3"`},
			want:    nil,
		},
		{
			name:    "codex model unaffected by a different config key ending in model",
			agent:   types.AgentCodex,
			profile: Profile{Model: "gpt-5.4"},
			raw:     []string{"-c", `fallback_model="o3"`},
			want:    []string{"-m", "gpt-5.4"},
		},
		{
			name:    "codex model unaffected by model assignment text inside another config value",
			agent:   types.AgentCodex,
			profile: Profile{Model: "gpt-5.4"},
			raw:     []string{"-c", `developer_instructions="use model=o3"`},
			want:    []string{"-m", "gpt-5.4"},
		},
		{
			name:    "codex effort unaffected by a neighbouring model config key",
			agent:   types.AgentCodex,
			profile: Profile{Effort: EffortLow},
			raw:     []string{"-c", `model_provider="azure"`},
			want:    []string{"-c", `model_reasoning_effort="low"`},
		},
		{
			name:    "codex effort pinned by config override",
			agent:   types.AgentCodex,
			profile: Profile{Model: "gpt-5.4", Effort: EffortLow},
			raw:     []string{"-c", `model_reasoning_effort="high"`},
			want:    []string{"-m", "gpt-5.4"},
		},
		{
			name:    "grok effort pinned through its alias spelling",
			agent:   types.AgentGrok,
			profile: Profile{Effort: EffortLow},
			raw:     []string{"--effort", "high"},
			want:    nil,
		},
		{
			name:    "copilot effort pinned through its alias spelling",
			agent:   types.AgentCopilot,
			profile: Profile{Effort: EffortLow},
			raw:     []string{"--reasoning-effort", "high"},
			want:    nil,
		},
		{
			name:    "pi effort pinned by thinking",
			agent:   types.AgentPi,
			profile: Profile{Effort: EffortLow},
			raw:     []string{"--thinking", "max"},
			want:    nil,
		},
		{
			name:    "unrelated raw args never suppress a knob",
			agent:   types.AgentClaude,
			profile: Profile{Model: "sonnet", Effort: EffortHigh},
			raw:     []string{"--permission-mode", "acceptEdits"},
			want:    []string{"--model", "sonnet", "--effort", "high"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NativeArgs(tt.agent, tt.profile, tt.raw)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("NativeArgs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNativeArgsDoesNotMutateRawArgs(t *testing.T) {
	raw := []string{"--permission-mode", "acceptEdits"}
	before := append([]string(nil), raw...)
	NativeArgs(types.AgentClaude, Profile{Model: "sonnet"}, raw)
	if !reflect.DeepEqual(raw, before) {
		t.Fatalf("rawArgs mutated: %v, want %v", raw, before)
	}
}

// TestACPModelIsMappedThroughAcpx closes the gap that made ACP targets
// unpinnable: no-mistakes drives them through acpx, whose --model is a real
// mechanism.
func TestACPModelIsMappedThroughAcpx(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentCursor, "acp:gemini"} {
		got := NativeArgs(name, Profile{Model: "gpt-5"}, nil)
		want := []string{"--model", "gpt-5"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("NativeArgs(%s) = %v, want %v", name, got, want)
		}
		if mech := MechanismFor(name, KnobModel); mech != MechanismArgs {
			t.Errorf("MechanismFor(%s, model) = %s, want %s", name, mech, MechanismArgs)
		}
	}
}

// TestACPEffortIsRefusedNotDropped records the deliberately unmappable half of
// the ACP story: acpx has no reasoning-effort surface.
func TestACPEffortIsRefusedNotDropped(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentCursor, "acp:gemini"} {
		if mech := MechanismFor(name, KnobEffort); mech != MechanismUnsupported {
			t.Errorf("MechanismFor(%s, effort) = %s, want %s", name, mech, MechanismUnsupported)
		}
		err := Validate(name, Profile{Effort: EffortHigh})
		if err == nil {
			t.Fatalf("Validate(%s, effort) = nil, want error", name)
		}
		if !strings.Contains(err.Error(), "cannot express effort") {
			t.Errorf("Validate(%s) error = %v, want it to name the unexpressible knob", name, err)
		}
		// agent_args_override refuses ACP keys, so the escape hatch it offers
		// must be the one an ACP operator can actually use.
		if strings.Contains(err.Error(), "agent_args_override") {
			t.Errorf("Validate(%s) points at a setting that refuses ACP names: %v", name, err)
		}
		if !strings.Contains(err.Error(), "acp_registry_overrides") {
			t.Errorf("Validate(%s) error = %v, want the ACP escape hatch", name, err)
		}
	}
}

func TestValidateRefusesUnmappableKnobs(t *testing.T) {
	tests := []struct {
		agent   types.AgentName
		profile Profile
		want    string
	}{
		{types.AgentRovoDev, Profile{Model: "x"}, "cannot express model"},
		{types.AgentRovoDev, Profile{Effort: EffortHigh}, "cannot express effort"},
		{types.AgentAntigravity, Profile{Model: "x"}, "cannot express model"},
		{types.AgentAntigravity, Profile{Effort: EffortHigh}, "cannot express effort"},
		{types.AgentOpenCode, Profile{Model: "gpt-5"}, "provider/model"},
		{"nope", Profile{Model: "x"}, "unknown agent"},
	}
	for _, tt := range tests {
		err := Validate(tt.agent, tt.profile)
		if err == nil {
			t.Fatalf("Validate(%s, %+v) = nil, want error", tt.agent, tt.profile)
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Validate(%s) error = %v, want it to contain %q", tt.agent, err, tt.want)
		}
	}
	// A native harness with no mechanism can still be reached through raw args,
	// so its refusal names that surface with the agent's own key.
	err := Validate(types.AgentRovoDev, Profile{Model: "x"})
	if !strings.Contains(err.Error(), "agent_args_override.rovodev") {
		t.Errorf("Validate(rovodev) error = %v, want the raw-args escape hatch", err)
	}
}

func TestValidateAcceptsExpressibleProfiles(t *testing.T) {
	tests := []struct {
		agent   types.AgentName
		profile Profile
	}{
		{types.AgentClaude, Profile{Model: "sonnet", Effort: EffortMax}},
		{types.AgentCodex, Profile{Model: "gpt-5.4", Effort: EffortMinimal}},
		{types.AgentGrok, Profile{Effort: EffortHigh}},
		{types.AgentPi, Profile{Model: "anthropic/claude-sonnet", Effort: EffortXHigh}},
		{types.AgentCopilot, Profile{Model: "gpt-5.4"}},
		{types.AgentOpenCode, Profile{Model: "openai/gpt-5", Effort: EffortHigh}},
		{types.AgentCursor, Profile{Model: "gpt-5"}},
		{types.AgentRovoDev, Profile{}},
		{types.AgentAntigravity, Profile{}},
		{"nope", Profile{}},
	}
	for _, tt := range tests {
		if err := Validate(tt.agent, tt.profile); err != nil {
			t.Errorf("Validate(%s, %+v) = %v, want nil", tt.agent, tt.profile, err)
		}
	}
}

func TestValidateRejectsUnknownEffortValue(t *testing.T) {
	err := Validate(types.AgentClaude, Profile{Effort: Effort("hgh")})
	if err == nil || !strings.Contains(err.Error(), "invalid effort") {
		t.Fatalf("Validate = %v, want invalid effort", err)
	}
}

func TestParseEffort(t *testing.T) {
	for _, want := range Efforts() {
		got, err := ParseEffort(" " + string(want) + " ")
		if err != nil || got != want {
			t.Errorf("ParseEffort(%q) = (%q, %v)", want, got, err)
		}
	}
	if got, err := ParseEffort(""); err != nil || got != "" {
		t.Errorf("ParseEffort(\"\") = (%q, %v), want empty", got, err)
	}
	if _, err := ParseEffort("turbo"); err == nil {
		t.Fatal("ParseEffort(turbo) = nil error, want rejection")
	}
}

func TestSplitProviderModel(t *testing.T) {
	provider, id, ok := SplitProviderModel("openai/gpt-5")
	if !ok || provider != "openai" || id != "gpt-5" {
		t.Fatalf("SplitProviderModel = (%q, %q, %v)", provider, id, ok)
	}
	for _, bad := range []string{"gpt-5", "/gpt-5", "openai/", "", "   "} {
		if _, _, ok := SplitProviderModel(bad); ok {
			t.Errorf("SplitProviderModel(%q) accepted, want rejection", bad)
		}
	}
}

func TestServedMatchesRequested(t *testing.T) {
	tests := []struct {
		name           string
		requested      string
		served         string
		servedProvider string
		want           bool
	}{
		{name: "bare request matches bare served", requested: "grok-4.6", served: "grok-4.6", servedProvider: "xai", want: true},
		{name: "qualified request matches pi split fields", requested: "xai/grok-4.6", served: "grok-4.6", servedProvider: "xai", want: true},
		{name: "true mismatch different model", requested: "xai/grok-4.6", served: "grok-4.5", servedProvider: "xai", want: false},
		{name: "true mismatch different provider", requested: "openai/grok-4.6", served: "grok-4.6", servedProvider: "xai", want: false},
		{name: "qualified request rejects empty provider", requested: "xai/grok-4.6", served: "grok-4.6", servedProvider: "", want: false},
		{name: "qualified served matches qualified request", requested: "xai/grok-4.6", served: "xai/grok-4.6", want: true},
		{name: "qualified served rejects contradictory provider metadata", requested: "xai/grok-4.6", served: "xai/grok-4.6", servedProvider: "openai", want: false},
		{name: "bare request matches qualified served id", requested: "grok-4.6", served: "xai/grok-4.6", want: true},
		{name: "bare request rejects contradictory provider metadata", requested: "grok-4.6", served: "xai/grok-4.6", servedProvider: "openai", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ServedMatchesRequested(tt.requested, tt.served, tt.servedProvider); got != tt.want {
				t.Fatalf("ServedMatchesRequested(%q, %q, %q) = %v, want %v", tt.requested, tt.served, tt.servedProvider, got, tt.want)
			}
		})
	}
}

// TestEverySupportedAgentHasAMapping keeps a newly added agent from silently
// inheriting "unknown agent" behavior instead of a deliberate decision.
func TestEverySupportedAgentHasAMapping(t *testing.T) {
	for _, name := range []types.AgentName{
		types.AgentClaude, types.AgentCodex, types.AgentGrok, types.AgentRovoDev,
		types.AgentOpenCode, types.AgentPi, types.AgentCopilot, types.AgentAntigravity,
		types.AgentCursor, "acp:gemini",
	} {
		if !Known(name) {
			t.Errorf("agent %q has no entry in the agentcfg mapping", name)
		}
	}
}

func TestProfileString(t *testing.T) {
	if got := (Profile{}).String(); got != "" {
		t.Errorf("zero Profile.String() = %q, want empty", got)
	}
	if got := (Profile{Model: "gpt-5.4"}).String(); got != "model=gpt-5.4" {
		t.Errorf("Profile.String() = %q", got)
	}
	if got := (Profile{Model: "gpt-5.4", Effort: EffortLow}).String(); got != "model=gpt-5.4,effort=low" {
		t.Errorf("Profile.String() = %q", got)
	}
}
