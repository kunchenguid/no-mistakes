package config

import (
	"strings"
	"testing"
)

const freeModelFleetYAML = `
minimax_extra_review:
  enabled: false
  model: opencode-go/minimax-m3.1-flash-free
  fallback_model: opencode-go/muse-spark
  mode: advisory
  steps: [review]
  repos: [skills, checks, tui-dash, ru-compliance]
beam_drafts:
  enabled: false
  model: opencode-go/beam-free
  fallback_model: opencode-go/muse-spark
  steps: [commit, document]
  empty_retries: 1
  apply_check: true
  repos: [skills, checks, tui-dash, ru-compliance]
`

const freeModelAllowYAML = `
minimax_extra_review:
  enabled: true
  model: m
  fallback_model: f
  mode: advisory
  steps: [review]
  repos: [alpha]
beam_drafts:
  enabled: true
  model: m
  fallback_model: f
  steps: [commit, document]
  repos: [alpha]
`

const freeModelRepoYAML = `
minimax_extra_review:
  enabled: true
  model: m
  fallback_model: f
  mode: advisory
  steps: [review]
  repos: [a]
beam_drafts:
  enabled: true
  model: m
  fallback_model: f
  steps: [commit, document]
  repos: [a]
`

func TestFreeModels_DefaultOff(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("{}" + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinimaxExtraReview.Enabled || cfg.BeamDrafts.Enabled {
		t.Fatalf("free models default on")
	}
	if cfg.MinimaxExtraReview.Allows("https://github.com/o/skills") || cfg.BeamDrafts.Allows("https://github.com/o/skills") {
		t.Fatal("disabled settings allow a repo")
	}
}

func TestFreeModels_FleetShapeParses(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte(freeModelFleetYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinimaxExtraReview.Model != "opencode-go/minimax-m3.1-flash-free" {
		t.Fatalf("minimax model = %q", cfg.MinimaxExtraReview.Model)
	}
	if cfg.BeamDrafts.EmptyRetries != 1 || !cfg.BeamDrafts.ApplyCheck {
		t.Fatalf("beam guards = %+v", cfg.BeamDrafts)
	}
	if cfg.MinimaxExtraReview.Allows("https://github.com/o/skills") {
		t.Fatal("disabled minimax allows a repo")
	}
	merged := Merge(cfg, &RepoConfig{})
	if merged.MinimaxExtraReview.Model != cfg.MinimaxExtraReview.Model || merged.BeamDrafts.Model != cfg.BeamDrafts.Model {
		t.Fatalf("merge dropped free model settings")
	}
}

func TestFreeModels_Allowlist(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte(freeModelAllowYAML))
	if err != nil {
		t.Fatal(err)
	}
	urls := map[string]bool{
		"https://github.com/o/alpha":       true,
		"https://github.com/o/alpha.git":   true,
		"https://github.com/o/alpha/":      true,
		"git@github.com:o/alpha.git":       true,
		"https://github.com/o/beta":        false,
		"https://github.com/o/alpha-extra": false,
		"":                                 false,
	}
	for url, want := range urls {
		if got := cfg.MinimaxExtraReview.Allows(url); got != want {
			t.Errorf("minimax Allows(%q) = %v, want %v", url, got, want)
		}
		if got := cfg.BeamDrafts.Allows(url); got != want {
			t.Errorf("beam Allows(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestFreeModels_InvalidShapesFailClosed(t *testing.T) {
	join := func(lines ...string) string {
		out := ""
		for _, line := range lines {
			out += line + "\n"
		}
		return out
	}
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"minimax blocking mode", join("minimax_extra_review:", "  model: m", "  fallback_model: f", "  mode: blocking", "  steps: [review]", "  repos: [a]"), "mode"},
		{"minimax wrong steps", join("minimax_extra_review:", "  model: m", "  fallback_model: f", "  mode: advisory", "  steps: [document]", "  repos: [a]"), "steps"},
		{"minimax empty model", join("minimax_extra_review:", "  fallback_model: f", "  mode: advisory", "  steps: [review]", "  repos: [a]"), "model"},
		{"minimax enabled without repos", join("minimax_extra_review:", "  enabled: true", "  model: m", "  fallback_model: f", "  mode: advisory", "  steps: [review]"), "repos"},
		{"beam wrong steps", join("beam_drafts:", "  model: m", "  fallback_model: f", "  steps: [document]", "  repos: [a]"), "steps"},
		{"beam swapped steps", join("beam_drafts:", "  model: m", "  fallback_model: f", "  steps: [document, commit]", "  repos: [a]"), "steps"},
		{"beam no empty retry", join("beam_drafts:", "  model: m", "  fallback_model: f", "  steps: [commit, document]", "  empty_retries: 0", "  repos: [a]"), "empty_retries"},
		{"beam extra empty retry", join("beam_drafts:", "  model: m", "  fallback_model: f", "  steps: [commit, document]", "  empty_retries: 2", "  repos: [a]"), "empty_retries"},
		{"beam apply check off", join("beam_drafts:", "  model: m", "  fallback_model: f", "  steps: [commit, document]", "  apply_check: false", "  repos: [a]"), "apply_check"},
		{"beam enabled without repos", join("beam_drafts:", "  enabled: true", "  model: m", "  fallback_model: f", "  steps: [commit, document]", "  repos: []"), "repos"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadGlobalFromBytes([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadGlobalFromBytes error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFreeModels_RepoConfigCannotSet(t *testing.T) {
	repo, err := LoadRepoFromBytes([]byte(freeModelRepoYAML))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(DefaultGlobalConfig(), repo)
	if cfg.MinimaxExtraReview.Enabled || cfg.BeamDrafts.Enabled {
		t.Fatalf("repository config set free models")
	}
}
