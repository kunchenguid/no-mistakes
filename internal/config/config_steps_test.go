package config

import (
	"testing"
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

// Mapping-form entries (per-step options, custom steps) are a later PR; today
// they must be rejected with a clear error rather than silently misparsed.
func TestLoadRepoFromBytes_StepsMappingFormRejected(t *testing.T) {
	data := []byte("steps:\n  - name: rebase\n")
	if _, err := LoadRepoFromBytes(data); err == nil {
		t.Fatal("expected error for mapping-form steps entry")
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
