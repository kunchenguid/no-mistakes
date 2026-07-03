package config

import (
	"testing"
	"time"
)

func stepNames(specs []StepSpec) []string {
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	return names
}

func equalNames(got []StepSpec, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i, s := range got {
		if s.Name != want[i] {
			return false
		}
	}
	return true
}

func TestLoadRepoFromBytes_StepsScalarList(t *testing.T) {
	data := []byte("steps: [rebase, test, push, pr, ci]\n")
	cfg, err := LoadRepoFromBytes(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"rebase", "test", "push", "pr", "ci"}
	if !equalNames(cfg.Steps, want) {
		t.Errorf("steps = %v, want %v", stepNames(cfg.Steps), want)
	}
}

func TestLoadRepoFromBytes_StepsBlockList(t *testing.T) {
	data := []byte("steps:\n  - rebase\n  - lint\n  - push\n")
	cfg, err := LoadRepoFromBytes(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"rebase", "lint", "push"}
	if !equalNames(cfg.Steps, want) {
		t.Errorf("steps = %v, want %v", stepNames(cfg.Steps), want)
	}
}

func TestLoadRepoFromBytes_StepsAbsentIsNil(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("agent: claude\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Steps != nil {
		t.Errorf("steps = %v, want nil when absent", stepNames(cfg.Steps))
	}
}

// Mapping-form entries carry per-step options. A mapping with only a name is a
// built-in step with options (none here), parsed as that built-in.
func TestLoadRepoFromBytes_StepsMappingFormBuiltin(t *testing.T) {
	data := []byte("steps:\n  - name: rebase\n")
	cfg, err := LoadRepoFromBytes(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalNames(cfg.Steps, []string{"rebase"}) {
		t.Errorf("steps = %v, want [rebase]", stepNames(cfg.Steps))
	}
	if cfg.Steps[0].IsCommand() {
		t.Error("mapping without a command must not be a custom command step")
	}
}

// A mapping carrying a command is a custom command step, with its options
// parsed off the same entry.
func TestLoadRepoFromBytes_StepsCustomCommand(t *testing.T) {
	data := []byte("steps:\n" +
		"  - rebase\n" +
		"  - name: swiftlint\n" +
		"    command: swiftlint lint --quiet\n" +
		"    findings_json: build/swiftlint.json\n" +
		"    timeout: 5m\n" +
		"    auto_fix: true\n" +
		"    instructions:\n" +
		"      - .no-mistakes/swift.md\n" +
		"  - push\n")
	cfg, err := LoadRepoFromBytes(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !equalNames(cfg.Steps, []string{"rebase", "swiftlint", "push"}) {
		t.Fatalf("steps = %v", stepNames(cfg.Steps))
	}
	sl := cfg.Steps[1]
	if !sl.IsCommand() {
		t.Fatal("swiftlint entry should be a custom command step")
	}
	if sl.Command != "swiftlint lint --quiet" {
		t.Errorf("command = %q", sl.Command)
	}
	if sl.FindingsJSON != "build/swiftlint.json" {
		t.Errorf("findings_json = %q", sl.FindingsJSON)
	}
	if sl.Timeout != 5*time.Minute {
		t.Errorf("timeout = %v, want 5m", sl.Timeout)
	}
	if !sl.AutoFix {
		t.Error("auto_fix = false, want true")
	}
	if len(sl.Instructions) != 1 || sl.Instructions[0] != ".no-mistakes/swift.md" {
		t.Errorf("instructions = %v", sl.Instructions)
	}
}

// A malformed per-step timeout must be a clear parse error, not silently
// dropped.
func TestLoadRepoFromBytes_StepsInvalidTimeout(t *testing.T) {
	data := []byte("steps:\n  - name: swiftlint\n    command: swiftlint\n    timeout: not-a-duration\n")
	if _, err := LoadRepoFromBytes(data); err == nil {
		t.Fatal("expected error for invalid timeout")
	}
}

// steps selects which code executes, so it is a trusted-default-branch-only
// field exactly like commands and agent: from the pushed branch it is honored
// only under allow_repo_commands, else forced from the trusted copy.
func TestEffectiveRepoConfig_StepsFromTrustedCopy(t *testing.T) {
	pushed := &RepoConfig{Steps: []StepSpec{{Name: "push"}}}
	trusted := &RepoConfig{Steps: []StepSpec{{Name: "rebase"}, {Name: "push"}, {Name: "pr"}, {Name: "ci"}}}

	got := EffectiveRepoConfig(pushed, trusted, false)

	want := []string{"rebase", "push", "pr", "ci"}
	if !equalNames(got.Steps, want) {
		t.Errorf("steps = %v, want trusted value %v", stepNames(got.Steps), want)
	}
	// The pushed config must not be mutated.
	if !equalNames(pushed.Steps, []string{"push"}) {
		t.Errorf("pushed config was mutated: steps = %v", stepNames(pushed.Steps))
	}
}

func TestEffectiveRepoConfig_StepsFailClosedWithoutTrusted(t *testing.T) {
	pushed := &RepoConfig{Steps: []StepSpec{{Name: "push"}}}

	got := EffectiveRepoConfig(pushed, nil, false)

	if got.Steps != nil {
		t.Errorf("steps = %v, want nil (default pipeline) with no trusted config", stepNames(got.Steps))
	}
}

func TestEffectiveRepoConfig_StepsOptInHonorsPushed(t *testing.T) {
	pushed := &RepoConfig{Steps: []StepSpec{{Name: "rebase"}, {Name: "push"}}}
	trusted := &RepoConfig{Steps: []StepSpec{{Name: "rebase"}, {Name: "test"}, {Name: "push"}}}

	got := EffectiveRepoConfig(pushed, trusted, true)

	want := []string{"rebase", "push"}
	if !equalNames(got.Steps, want) {
		t.Errorf("steps = %v, want pushed value %v under opt-in", stepNames(got.Steps), want)
	}
}

// A custom command step's auto_fix flag drives its executor auto-fix limit,
// consistent with how built-in steps express fixability.
func TestAutoFixLimit_CustomCommandStep(t *testing.T) {
	cfg := &Config{
		AutoFix: autoFixDefaults(),
		Steps: []StepSpec{
			{Name: "swiftlint", Command: "swiftlint lint", AutoFix: true},
			{Name: "xctest", Command: "xcodebuild test"}, // auto_fix defaults false
		},
	}
	if got := cfg.AutoFixLimit("swiftlint"); got != DefaultCommandAutoFixLimit {
		t.Errorf("swiftlint auto-fix limit = %d, want %d", got, DefaultCommandAutoFixLimit)
	}
	if got := cfg.AutoFixLimit("xctest"); got != 0 {
		t.Errorf("xctest auto-fix limit = %d, want 0 (auto_fix off ⇒ parks)", got)
	}
	// A built-in step name is unaffected by the custom-step lookup.
	if got := cfg.AutoFixLimit("lint"); got != cfg.AutoFix.Lint {
		t.Errorf("built-in lint limit = %d, want %d", got, cfg.AutoFix.Lint)
	}
}

func TestMerge_StepsFromRepoOnly(t *testing.T) {
	global := &GlobalConfig{}
	repo := &RepoConfig{Steps: []StepSpec{{Name: "rebase"}, {Name: "push"}}}

	cfg := Merge(global, repo)

	if !equalNames(cfg.Steps, []string{"rebase", "push"}) {
		t.Errorf("steps = %v, want repo value", stepNames(cfg.Steps))
	}

	empty := Merge(&GlobalConfig{}, &RepoConfig{})
	if empty.Steps != nil {
		t.Errorf("steps = %v, want nil when repo config has none", stepNames(empty.Steps))
	}
}
