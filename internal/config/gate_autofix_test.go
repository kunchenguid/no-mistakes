package config

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestGateAutoFixConfig(t *testing.T) {
	const gates = "gates:\n  - name: mutation\n    after: test\n    command: make mutation\n"
	for _, tc := range []struct {
		name, yaml string
		want       int
		invalid    bool
	}{
		{"default", gates, 0, false},
		{"disabled", gates + "auto_fix:\n  gates: {mutation: 0}\n", 0, false},
		{"enabled", gates + "auto_fix:\n  gates: {mutation: 2}\n", 2, false},
		{"negative", gates + "auto_fix:\n  gates: {mutation: -1}\n", 0, true},
		{"unknown gate", gates + "auto_fix:\n  gates: {missing: 2}\n", 0, true},
		{"fraction", gates + "auto_fix:\n  gates: {mutation: 1.5}\n", 0, true},
		{"text", gates + "auto_fix:\n  gates: {mutation: nope}\n", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, err := LoadRepoFromBytes([]byte(tc.yaml))
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid budget accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			cfg := Merge(DefaultGlobalConfig(), repo)
			if got := cfg.AutoFixLimit(types.CustomGateStepName(types.StepTest, "mutation")); got != tc.want {
				t.Fatalf("budget = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestGateAutoFixTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{AutoFix: AutoFixRaw{Gates: map[string]int{"mutation": 99}}}
	trusted := &RepoConfig{AutoFix: AutoFixRaw{Gates: map[string]int{"mutation": 2}}}
	for _, allow := range []bool{false, true} {
		for _, source := range []*RepoConfig{trusted, nil} {
			effective := EffectiveRepoConfig(pushed, source, allow)
			global := DefaultGlobalConfig()
			global.AutoFix.Gates = map[string]int{"mutation": 88}
			cfg := Merge(global, effective)
			want := 0
			if source != nil {
				want = 2
			}
			if got := cfg.AutoFixLimit(types.CustomGateStepName(types.StepTest, "mutation")); got != want {
				t.Fatalf("allow=%v trusted=%v: budget=%d, want %d", allow, source != nil, got, want)
			}
			if source != nil {
				effective.AutoFix.Gates["mutation"] = 7
				cfg.AutoFix.Gates["mutation"] = 8
				if trusted.AutoFix.Gates["mutation"] != 2 {
					t.Fatal("trusted budget aliased")
				}
			}
		}
	}
}

func TestGateAutoFixRejectedGlobally(t *testing.T) {
	for _, value := range []string{"{mutation: 2}", "{}", "null"} {
		if _, err := LoadGlobalFromBytes([]byte("auto_fix:\n  gates: " + value + "\n")); err == nil {
			t.Fatalf("global auto_fix.gates=%s accepted", value)
		}
	}
}
