package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLoadGlobalDefaultsRoutingWhenAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("agent: codex\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if cfg.Routing.IsZero() {
		t.Fatal("expected default routing when the block is absent")
	}
	if err := cfg.Routing.Validate(); err != nil {
		t.Fatalf("default routing invalid: %v", err)
	}
}

func TestLoadGlobalMissingFileDefaultsRouting(t *testing.T) {
	cfg, err := LoadGlobal(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if err := cfg.Routing.Validate(); err != nil {
		t.Fatalf("default routing invalid: %v", err)
	}
}

func TestLoadGlobalRejectsInvalidRoutingBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	// A present routing block fully replaces the defaults and must be
	// complete and valid; this one is incomplete (no routes).
	data := `routing:
  runners:
    codex: {executable: codex, failure_domain: openai}
  profiles:
    review_strong:
      candidates:
        - {runner: codex, model: gpt-5.6-sol, effort: high}
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGlobal(path); err == nil {
		t.Fatal("expected an incomplete routing block to be rejected at load")
	}
}

func TestLoadGlobalRejectsPresentButEmptyRoutingBlock(t *testing.T) {
	for _, block := range []string{"routing: {}\n", "routing:\n  routes: {}\n", "routing: null\n", "routing:\n"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(block), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadGlobal(path); err == nil {
			t.Fatalf("expected a present-but-empty routing block to be rejected:\n%s", block)
		}
	}
}

func TestParseRepoConfigRejectsExecutionMechanics(t *testing.T) {
	for _, mechanic := range []string{
		"runners:\n  codex: {executable: codex, failure_domain: openai}\n",
		"profiles:\n  fix_fast:\n    candidates:\n      - {runner: codex, model: gpt-5.6-luna, effort: medium}\n",
		"routing:\n  runners:\n    codex: {executable: codex, failure_domain: openai}\n",
	} {
		if _, err := LoadRepoFromBytes([]byte(mechanic)); err == nil {
			t.Fatalf("expected repo execution-mechanic definition to be rejected:\n%s", mechanic)
		}
	}
}

func TestLoadRepoFromBytesParsesRoutes(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("routes:\n  initial_review: authority_strong\n"))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes: %v", err)
	}
	if got := cfg.Routes[types.PurposeInitialReview]; got != ProfileAuthorityStrong {
		t.Fatalf("repo route = %q, want %q", got, ProfileAuthorityStrong)
	}
}

func TestEffectiveRepoConfigRoutesComeFromTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{Routes: map[types.Purpose]ProfileName{types.PurposeInitialReview: ProfileFixFast}}
	trusted := &RepoConfig{Routes: map[types.Purpose]ProfileName{types.PurposeInitialReview: ProfileProseFast}}

	got := EffectiveRepoConfig(pushed, trusted, false)
	if got.Routes[types.PurposeInitialReview] != ProfileProseFast {
		t.Fatalf("effective route = %q, want trusted %q", got.Routes[types.PurposeInitialReview], ProfileProseFast)
	}
	if pushed.Routes[types.PurposeInitialReview] != ProfileFixFast {
		t.Fatal("pushed config was mutated")
	}
}

func TestEffectiveRepoConfigRoutesIgnorePushedEvenWithOptIn(t *testing.T) {
	pushed := &RepoConfig{Routes: map[types.Purpose]ProfileName{types.PurposeInitialReview: ProfileFixFast}}
	trusted := &RepoConfig{Routes: map[types.Purpose]ProfileName{types.PurposeInitialReview: ProfileProseFast}}

	// allow_repo_commands opts pushed commands/agent in, but routes must
	// still come only from the trusted copy.
	got := EffectiveRepoConfig(pushed, trusted, true)
	if got.Routes[types.PurposeInitialReview] != ProfileProseFast {
		t.Fatalf("effective route with opt-in = %q, want trusted %q", got.Routes[types.PurposeInitialReview], ProfileProseFast)
	}
}

func TestEffectiveRepoConfigNoTrustedRoutesEmpty(t *testing.T) {
	pushed := &RepoConfig{Routes: map[types.Purpose]ProfileName{types.PurposeInitialReview: ProfileFixFast}}
	got := EffectiveRepoConfig(pushed, nil, false)
	if len(got.Routes) != 0 {
		t.Fatalf("effective routes = %v, want empty when no trusted copy", got.Routes)
	}
}

func TestMergeAppliesRepoRouteOverride(t *testing.T) {
	global := &GlobalConfig{Routing: DefaultRoutingConfig()}
	repo := &RepoConfig{Routes: map[types.Purpose]ProfileName{types.PurposeInitialReview: ProfileAuthorityStrong}}

	cfg := Merge(global, repo)
	if err := cfg.ValidateRouting(); err != nil {
		t.Fatalf("ValidateRouting after override: %v", err)
	}
	profiles, err := cfg.Routing.ResolveRoute(types.PurposeInitialReview)
	if err != nil {
		t.Fatalf("ResolveRoute: %v", err)
	}
	if len(profiles) != 1 || profiles[0].Name != ProfileAuthorityStrong {
		t.Fatalf("overridden route = %v, want [authority_strong]", profiles)
	}
	// The shared default routing must not be mutated by the override.
	base := DefaultRoutingConfig()
	if base.Routes[types.PurposeInitialReview][0] != ProfileReviewStrong {
		t.Fatal("default routing was mutated by a repo override")
	}
}

func TestMergeRepoRouteToUnknownProfileFailsValidation(t *testing.T) {
	global := &GlobalConfig{Routing: DefaultRoutingConfig()}
	repo := &RepoConfig{Routes: map[types.Purpose]ProfileName{types.PurposeInitialReview: ProfileName("ghost")}}
	cfg := Merge(global, repo)
	if err := cfg.ValidateRouting(); err == nil {
		t.Fatal("expected repo override to an unknown profile to fail validation")
	}
}

func TestMergeRepoRouteForUnregisteredPurposeFailsValidation(t *testing.T) {
	global := &GlobalConfig{Routing: DefaultRoutingConfig()}
	repo := &RepoConfig{Routes: map[types.Purpose]ProfileName{types.Purpose("bogus"): ProfileReviewStrong}}
	cfg := Merge(global, repo)
	if err := cfg.ValidateRouting(); err == nil {
		t.Fatal("expected repo override for an unregistered purpose to fail validation")
	}
}

func TestMergeDefaultsRoutingWhenGlobalZero(t *testing.T) {
	cfg := Merge(&GlobalConfig{}, &RepoConfig{})
	if err := cfg.ValidateRouting(); err != nil {
		t.Fatalf("ValidateRouting with zero global routing: %v", err)
	}
}
