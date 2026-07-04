package config

import (
	"testing"
	"time"
)

// A pushed branch must never set, switch, or drop the shared gate profile:
// EffectiveRepoConfig takes Profile only from the trusted default-branch copy,
// and does so even under allow_repo_commands (the trusted-only-in-v1 rule that
// keeps a contributor from steering which shared commands/prompts run).
func TestEffectiveRepoConfig_ProfileTrustedOnly(t *testing.T) {
	t.Run("pushed_cannot_set_when_trusted_absent", func(t *testing.T) {
		pushed := &RepoConfig{Profile: "attacker"}
		got := EffectiveRepoConfig(pushed, &RepoConfig{}, false)
		if got.Profile != "" {
			t.Errorf("profile = %q, want empty; a pushed branch cannot set a profile the trusted copy did not", got.Profile)
		}
	})

	t.Run("pushed_cannot_switch_trusted_profile", func(t *testing.T) {
		pushed := &RepoConfig{Profile: "lax"}
		trusted := &RepoConfig{Profile: "team-ios"}
		got := EffectiveRepoConfig(pushed, trusted, false)
		if got.Profile != "team-ios" {
			t.Errorf("profile = %q, want trusted value %q", got.Profile, "team-ios")
		}
	})

	t.Run("pushed_cannot_remove_trusted_profile", func(t *testing.T) {
		pushed := &RepoConfig{Profile: ""}
		trusted := &RepoConfig{Profile: "team-ios"}
		got := EffectiveRepoConfig(pushed, trusted, false)
		if got.Profile != "team-ios" {
			t.Errorf("profile = %q, want trusted value %q; a pushed branch cannot drop the profile", got.Profile, "team-ios")
		}
	})

	t.Run("trusted_only_even_under_allow_repo_commands", func(t *testing.T) {
		// The safer v1 default: unlike commands/agent/steps, profile stays
		// trusted-only even when the maintainer opted into pushed-branch
		// commands wholesale.
		pushed := &RepoConfig{Profile: "attacker"}
		trusted := &RepoConfig{Profile: "team-ios"}
		got := EffectiveRepoConfig(pushed, trusted, true)
		if got.Profile != "team-ios" {
			t.Errorf("profile = %q, want trusted value %q even under allow_repo_commands", got.Profile, "team-ios")
		}
	})

	t.Run("no_trusted_forces_empty_under_opt_in", func(t *testing.T) {
		pushed := &RepoConfig{Profile: "attacker"}
		got := EffectiveRepoConfig(pushed, nil, true)
		if got.Profile != "" {
			t.Errorf("profile = %q, want empty with no trusted copy (fail closed)", got.Profile)
		}
	})

	t.Run("pushed_not_mutated", func(t *testing.T) {
		pushed := &RepoConfig{Profile: "attacker"}
		_ = EffectiveRepoConfig(pushed, &RepoConfig{Profile: "team-ios"}, false)
		if pushed.Profile != "attacker" {
			t.Errorf("pushed config was mutated: profile = %q", pushed.Profile)
		}
	})
}

func TestLoadProfileFromBytes(t *testing.T) {
	data := []byte(`version: 3
steps:
  - rebase
  - name: ios-review
    type: skill
    skill: skills/ios-review.md
    mode: review
  - name: swiftlint
    type: command
    command: swiftlint --strict
    timeout: 5m
  - push
`)
	profile, err := LoadProfileFromBytes(data)
	if err != nil {
		t.Fatalf("LoadProfileFromBytes: %v", err)
	}
	if profile.Version != 3 {
		t.Errorf("version = %d, want 3", profile.Version)
	}
	if len(profile.Steps) != 4 {
		t.Fatalf("steps = %d, want 4", len(profile.Steps))
	}
	if !profile.Steps[1].IsSkill() || profile.Steps[1].Skill != "skills/ios-review.md" {
		t.Errorf("step[1] not the expected skill step: %+v", profile.Steps[1])
	}
	if !profile.Steps[2].IsCommand() || profile.Steps[2].Timeout != 5*time.Minute {
		t.Errorf("step[2] not the expected command step: %+v", profile.Steps[2])
	}
}

func TestLoadProfileFromBytes_Invalid(t *testing.T) {
	if _, err := LoadProfileFromBytes([]byte("steps: [oops\n")); err == nil {
		t.Fatal("expected a parse error for malformed profile.yaml")
	}
}

func TestStepSpec_UseUnmarshal(t *testing.T) {
	cfg, err := parseRepoConfig([]byte("steps:\n  - use: profile\n  - push\n"))
	if err != nil {
		t.Fatalf("parseRepoConfig: %v", err)
	}
	if len(cfg.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(cfg.Steps))
	}
	if !cfg.Steps[0].IsProfileSplice() {
		t.Errorf("step[0] should be the profile splice sentinel: %+v", cfg.Steps[0])
	}
	if cfg.Steps[1].IsProfileSplice() {
		t.Errorf("step[1] should not be a splice sentinel: %+v", cfg.Steps[1])
	}
}

func TestComposeProfileSteps(t *testing.T) {
	profileSteps := []StepSpec{{Name: "rebase"}, {Name: "review"}, {Name: "push"}}

	t.Run("no_repo_steps_uses_profile_as_is", func(t *testing.T) {
		got, err := ComposeProfileSteps(nil, profileSteps)
		if err != nil {
			t.Fatalf("compose: %v", err)
		}
		if !equalNames(got, []string{"rebase", "review", "push"}) {
			t.Errorf("merged = %v, want the profile list", stepNames(got))
		}
	})

	t.Run("splice_expands_in_place", func(t *testing.T) {
		repoSteps := []StepSpec{
			{Use: "profile"},
			{Name: "repo-check", Command: "./check.sh"},
		}
		got, err := ComposeProfileSteps(repoSteps, profileSteps)
		if err != nil {
			t.Fatalf("compose: %v", err)
		}
		if !equalNames(got, []string{"rebase", "review", "push", "repo-check"}) {
			t.Errorf("merged = %v, want profile spliced before repo-check", stepNames(got))
		}
		// The sentinel must not survive into the built pipeline.
		for _, s := range got {
			if s.IsProfileSplice() {
				t.Errorf("splice sentinel leaked into merged list: %+v", s)
			}
		}
	})

	t.Run("splice_in_the_middle", func(t *testing.T) {
		repoSteps := []StepSpec{
			{Name: "intent"},
			{Use: "profile"},
			{Name: "repo-check", Command: "./check.sh"},
		}
		got, err := ComposeProfileSteps(repoSteps, profileSteps)
		if err != nil {
			t.Fatalf("compose: %v", err)
		}
		if !equalNames(got, []string{"intent", "rebase", "review", "push", "repo-check"}) {
			t.Errorf("merged = %v, want profile spliced in the middle", stepNames(got))
		}
	})

	t.Run("repo_steps_without_sentinel_is_error", func(t *testing.T) {
		repoSteps := []StepSpec{{Name: "push"}}
		if _, err := ComposeProfileSteps(repoSteps, profileSteps); err == nil {
			t.Fatal("expected an error when repo has steps but no `use: profile` sentinel")
		}
	})

	t.Run("multiple_sentinels_is_error", func(t *testing.T) {
		repoSteps := []StepSpec{{Use: "profile"}, {Use: "profile"}}
		if _, err := ComposeProfileSteps(repoSteps, profileSteps); err == nil {
			t.Fatal("expected an error for more than one splice sentinel")
		}
	})

	t.Run("unknown_use_value_is_error", func(t *testing.T) {
		repoSteps := []StepSpec{{Use: "profiles"}}
		if _, err := ComposeProfileSteps(repoSteps, profileSteps); err == nil {
			t.Fatal("expected an error for an unknown use: value")
		}
	})
}

func TestHasProfileSplice(t *testing.T) {
	if HasProfileSplice([]StepSpec{{Name: "push"}}) {
		t.Error("no sentinel present, want false")
	}
	if !HasProfileSplice([]StepSpec{{Name: "push"}, {Use: "profile"}}) {
		t.Error("sentinel present, want true")
	}
}
