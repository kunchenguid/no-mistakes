package config

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// okLookPath is a lookPath stub that pretends every binary exists. ResolveReviewers
// only consults lookPath for rovodev, but tests pass it for completeness.
func okLookPath(bin string) (string, error) { return bin, nil }

func TestLoadGlobal_ReviewParsesUnderStrictKnownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `agent: claude
review:
  reviewers:
    - agent: codex
    - agent: claude
      args:
        - -m
        - opus
      path: /opt/claude
  max_parallel: 2
  fail_open: true
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal() error = %v", err)
	}
	if len(cfg.Review.Reviewers) != 2 {
		t.Fatalf("reviewers = %d, want 2", len(cfg.Review.Reviewers))
	}
	if cfg.Review.Reviewers[0].Agent != types.AgentCodex {
		t.Errorf("reviewers[0].Agent = %q, want codex", cfg.Review.Reviewers[0].Agent)
	}
	if cfg.Review.Reviewers[1].Agent != types.AgentClaude {
		t.Errorf("reviewers[1].Agent = %q, want claude", cfg.Review.Reviewers[1].Agent)
	}
	if got := cfg.Review.Reviewers[1].Args; !reflect.DeepEqual(got, []string{"-m", "opus"}) {
		t.Errorf("reviewers[1].Args = %v, want [-m opus]", got)
	}
	if cfg.Review.Reviewers[1].Path != "/opt/claude" {
		t.Errorf("reviewers[1].Path = %q, want /opt/claude", cfg.Review.Reviewers[1].Path)
	}
	if cfg.Review.MaxParallel != 2 {
		t.Errorf("max_parallel = %d, want 2", cfg.Review.MaxParallel)
	}
	if cfg.Review.FailOpen == nil || !*cfg.Review.FailOpen {
		t.Errorf("fail_open = %v, want true", cfg.Review.FailOpen)
	}
}

func TestLoadGlobal_ReviewUnknownKeyTripsKnownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `review:
  reviewers:
    - agent: codex
  bogus: true
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGlobal(path); err == nil {
		t.Fatal("expected error: unknown key under review must trip KnownFields(true)")
	}
}

func TestLoadGlobal_ReviewUnknownReviewerKeyTripsKnownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `review:
  reviewers:
    - agent: codex
      model: opus
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGlobal(path); err == nil {
		t.Fatal("expected error: unknown reviewer key must trip KnownFields(true)")
	}
}

func TestLoadGlobal_ReviewRejectsUnknownFamily(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `review:
  reviewers:
    - agent: gpt5
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatal("expected error for unknown reviewer family")
	}
	if !strings.Contains(err.Error(), "gpt5") {
		t.Errorf("expected error to mention unknown family, got: %v", err)
	}
}

func TestMerge_ReviewDefaultsEmptySingleAgentFallback(t *testing.T) {
	global := &GlobalConfig{Agent: types.AgentClaude}
	repo := &RepoConfig{}

	cfg := Merge(global, repo)

	if len(cfg.Review.Reviewers) != 0 {
		t.Errorf("reviewers = %d, want 0 (single-agent fallback)", len(cfg.Review.Reviewers))
	}
	if cfg.Review.FailOpen {
		t.Errorf("fail_open = true, want false by default")
	}
}

func TestMerge_ReviewFromGlobal(t *testing.T) {
	failOpen := true
	global := &GlobalConfig{
		Agent: types.AgentClaude,
		Review: ReviewRaw{
			Reviewers:   []ReviewerSpec{{Agent: types.AgentCodex}},
			MaxParallel: 3,
			FailOpen:    &failOpen,
		},
	}
	repo := &RepoConfig{}

	cfg := Merge(global, repo)

	if len(cfg.Review.Reviewers) != 1 || cfg.Review.Reviewers[0].Agent != types.AgentCodex {
		t.Errorf("reviewers = %v, want [codex] from global", cfg.Review.Reviewers)
	}
	if cfg.Review.MaxParallel != 3 {
		t.Errorf("max_parallel = %d, want 3", cfg.Review.MaxParallel)
	}
	if !cfg.Review.FailOpen {
		t.Errorf("fail_open = false, want true from global")
	}
}

func TestMerge_ReviewRepoOverridesGlobal(t *testing.T) {
	global := &GlobalConfig{
		Agent: types.AgentClaude,
		Review: ReviewRaw{
			Reviewers:   []ReviewerSpec{{Agent: types.AgentCodex}},
			MaxParallel: 1,
		},
	}
	repo := &RepoConfig{
		Review: ReviewRaw{
			Reviewers:   []ReviewerSpec{{Agent: types.AgentClaude}, {Agent: types.AgentPi}},
			MaxParallel: 5,
		},
	}

	cfg := Merge(global, repo)

	if len(cfg.Review.Reviewers) != 2 {
		t.Fatalf("reviewers = %d, want 2 (repo override)", len(cfg.Review.Reviewers))
	}
	if cfg.Review.Reviewers[0].Agent != types.AgentClaude || cfg.Review.Reviewers[1].Agent != types.AgentPi {
		t.Errorf("reviewers = %v, want [claude pi] from repo", cfg.Review.Reviewers)
	}
	if cfg.Review.MaxParallel != 5 {
		t.Errorf("max_parallel = %d, want 5 from repo", cfg.Review.MaxParallel)
	}
}

// TestEffectiveRepoConfig_StripsPushedReview proves the security gate: a review
// panel pushed on a feature branch must never win - the effective panel comes
// from the trusted default-branch copy (or is empty when there is no trusted
// copy), because reviewers select which binaries launch with maintainer creds.
func TestEffectiveRepoConfig_StripsPushedReview(t *testing.T) {
	pushed := &RepoConfig{
		Review: ReviewRaw{
			Reviewers: []ReviewerSpec{{Agent: types.AgentCodex, Path: "/tmp/evil"}},
		},
	}
	trusted := &RepoConfig{
		Review: ReviewRaw{
			Reviewers: []ReviewerSpec{{Agent: types.AgentClaude}},
		},
	}

	got := EffectiveRepoConfig(pushed, trusted, false)

	if len(got.Review.Reviewers) != 1 || got.Review.Reviewers[0].Agent != types.AgentClaude {
		t.Errorf("review = %v, want trusted [claude]", got.Review.Reviewers)
	}
	if got.Review.Reviewers[0].Path != "" {
		t.Errorf("review path = %q, want trusted (empty), not pushed /tmp/evil", got.Review.Reviewers[0].Path)
	}
	// The pushed config must not be mutated.
	if pushed.Review.Reviewers[0].Path != "/tmp/evil" {
		t.Errorf("pushed config was mutated: path = %q", pushed.Review.Reviewers[0].Path)
	}
}

func TestEffectiveRepoConfig_NoTrustedDisablesReview(t *testing.T) {
	pushed := &RepoConfig{
		Review: ReviewRaw{
			Reviewers: []ReviewerSpec{{Agent: types.AgentCodex}},
		},
	}

	got := EffectiveRepoConfig(pushed, nil, false)

	if len(got.Review.Reviewers) != 0 {
		t.Errorf("review = %v, want empty (no trusted config)", got.Review.Reviewers)
	}
}

func TestEffectiveRepoConfig_OptInHonorsPushedReview(t *testing.T) {
	pushed := &RepoConfig{
		Review: ReviewRaw{
			Reviewers: []ReviewerSpec{{Agent: types.AgentCodex}},
		},
	}
	trusted := &RepoConfig{
		Review: ReviewRaw{
			Reviewers: []ReviewerSpec{{Agent: types.AgentClaude}},
		},
	}

	got := EffectiveRepoConfig(pushed, trusted, true)

	if len(got.Review.Reviewers) != 1 || got.Review.Reviewers[0].Agent != types.AgentCodex {
		t.Errorf("review = %v, want pushed [codex] under opt-in", got.Review.Reviewers)
	}
}

func TestLoadPushedRepo_DoesNotValidateStrippedReview(t *testing.T) {
	dir := t.TempDir()
	repoYAML := "review:\n  reviewers:\n    - agent: gpt5\n"
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(repoYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadPushedRepo(dir)
	if err != nil {
		t.Fatalf("LoadPushedRepo should parse pushed review config before trust filtering, got: %v", err)
	}
	if len(cfg.Review.Reviewers) != 0 {
		t.Fatalf("pushed review config = %+v, want stripped review panel", cfg.Review.Reviewers)
	}

	effective := EffectiveRepoConfig(cfg, nil, false)
	if err := ValidateReview(effective.Review); err != nil {
		t.Fatalf("stripped pushed review should validate after trust filtering, got: %v", err)
	}
}

func TestLoadPushedRepo_DoesNotParseMalformedStrippedReview(t *testing.T) {
	dir := t.TempDir()
	repoYAML := "ignore_patterns:\n  - '*.generated.go'\nreview: bad\n"
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(repoYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadPushedRepo(dir)
	if err != nil {
		t.Fatalf("LoadPushedRepo should ignore malformed pushed review config before trust filtering, got: %v", err)
	}
	if len(cfg.Review.Reviewers) != 0 {
		t.Fatalf("pushed review config = %+v, want stripped review panel", cfg.Review.Reviewers)
	}
	if len(cfg.IgnorePatterns) != 1 || cfg.IgnorePatterns[0] != "*.generated.go" {
		t.Fatalf("ignore_patterns = %v, want pushed non-executing fields preserved", cfg.IgnorePatterns)
	}
}

func TestValidateReview_RejectsEffectiveInvalidReview(t *testing.T) {
	err := ValidateReview(ReviewRaw{Reviewers: []ReviewerSpec{{Agent: "gpt5"}}})
	if err == nil {
		t.Fatal("expected invalid effective review panel to be rejected")
	}
	if !strings.Contains(err.Error(), "gpt5") {
		t.Errorf("expected error to mention gpt5, got: %v", err)
	}
}

func TestLoadRepo_ReviewValidatesReservedArgs(t *testing.T) {
	dir := t.TempDir()
	repoYAML := "review:\n  reviewers:\n    - agent: codex\n      args:\n        - --json\n"
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(repoYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadRepo(dir)
	if err == nil {
		t.Fatal("expected error: repo-level reviewer with reserved arg --json must be rejected")
	}
	if !strings.Contains(err.Error(), "managed by no-mistakes") {
		t.Errorf("expected 'managed by no-mistakes' in error, got: %v", err)
	}
}

func TestLoadRepoFromBytes_ReviewValidatesUnknownFamily(t *testing.T) {
	data := []byte("review:\n  reviewers:\n    - agent: gpt5\n")
	_, err := LoadRepoFromBytes(data)
	if err == nil {
		t.Fatal("expected error: repo-level reviewer with unknown family must be rejected")
	}
	if !strings.Contains(err.Error(), "gpt5") {
		t.Errorf("expected error to mention unknown family, got: %v", err)
	}
}

func TestLoadRepoFromBytes_ReviewValidatesEmptyArg(t *testing.T) {
	data := []byte("review:\n  reviewers:\n    - agent: codex\n      args:\n        - \" \"\n")
	_, err := LoadRepoFromBytes(data)
	if err == nil {
		t.Fatal("expected error: repo-level reviewer with empty arg must be rejected")
	}
	if !strings.Contains(err.Error(), "empty arg") {
		t.Errorf("expected 'empty arg' in error, got: %v", err)
	}
}

func TestValidateReviewers_RejectsUnknownFamily(t *testing.T) {
	err := validateReviewers([]ReviewerSpec{{Agent: "gpt5"}})
	if err == nil {
		t.Fatal("expected error for unknown reviewer family")
	}
	if !strings.Contains(err.Error(), "gpt5") {
		t.Errorf("expected error to mention unknown family, got: %v", err)
	}
}

func TestValidateReviewers_RejectsMissingAgent(t *testing.T) {
	if err := validateReviewers([]ReviewerSpec{{}}); err == nil {
		t.Fatal("expected error for missing reviewer agent")
	}
}

func TestValidateReviewers_RejectsReservedArgs(t *testing.T) {
	cases := []struct {
		agent types.AgentName
		arg   string
	}{
		{types.AgentCodex, "exec"},
		{types.AgentCodex, "--json"},
		{types.AgentClaude, "-p"},
		{types.AgentClaude, "--output-format=stream-json"},
		{types.AgentOpenCode, "serve"},
	}
	for _, tc := range cases {
		t.Run(string(tc.agent)+"_"+tc.arg, func(t *testing.T) {
			err := validateReviewers([]ReviewerSpec{{Agent: tc.agent, Args: []string{tc.arg}}})
			if err == nil {
				t.Fatalf("expected error for reserved arg %q on %q", tc.arg, tc.agent)
			}
			if !strings.Contains(err.Error(), "managed by no-mistakes") {
				t.Errorf("expected 'managed by no-mistakes' in error, got: %v", err)
			}
		})
	}
}

func TestValidateReviewers_RejectsAutoArgs(t *testing.T) {
	err := validateReviewers([]ReviewerSpec{{Agent: types.AgentAuto, Args: []string{"--json"}}})
	if err == nil {
		t.Fatal("expected auto reviewer args to be rejected")
	}
	if !strings.Contains(err.Error(), "cannot use per-reviewer args") {
		t.Errorf("expected auto args error, got: %v", err)
	}
}

func TestValidateReviewers_AllowsAutoAndACP(t *testing.T) {
	specs := []ReviewerSpec{
		{Agent: types.AgentAuto},
		{Agent: "acp:gemini"},
		{Agent: types.AgentCodex},
	}
	if err := validateReviewers(specs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveReviewers_RejectsBareAutoWithoutConcreteAgent(t *testing.T) {
	cfg := &Config{
		Agent:  types.AgentAuto,
		Review: Review{Reviewers: []ReviewerSpec{{Agent: types.AgentAuto}}},
	}
	_, err := cfg.ResolveReviewers(context.Background(), okLookPath)
	if err == nil {
		t.Fatal("expected error: bare auto reviewer cannot resolve without a concrete agent")
	}
}

func TestResolveReviewers_ExpandsBareAutoToAgent(t *testing.T) {
	cfg := &Config{
		Agent:  types.AgentClaude,
		Review: Review{Reviewers: []ReviewerSpec{{Agent: types.AgentAuto}}},
	}
	got, err := cfg.ResolveReviewers(context.Background(), okLookPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Agent != types.AgentClaude {
		t.Errorf("resolved = %v, want [claude] (auto expanded to agent)", got)
	}
}

func TestResolveReviewers_Dedups(t *testing.T) {
	cfg := &Config{
		Agent: types.AgentClaude,
		Review: Review{Reviewers: []ReviewerSpec{
			{Agent: types.AgentCodex},
			{Agent: types.AgentClaude},
			{Agent: types.AgentCodex}, // duplicate of [0]
		}},
	}
	got, err := cfg.ResolveReviewers(context.Background(), okLookPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("resolved = %d reviewers, want 2 after dedup", len(got))
	}
	if got[0].Agent != types.AgentCodex || got[1].Agent != types.AgentClaude {
		t.Errorf("resolved = %v, want [codex claude] preserving first-occurrence order", got)
	}
}

func TestResolveReviewers_EmptyReturnsNil(t *testing.T) {
	cfg := &Config{Agent: types.AgentClaude}
	got, err := cfg.ResolveReviewers(context.Background(), okLookPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("resolved = %v, want nil for empty panel", got)
	}
}

func TestReviewerPathAndArgs_Precedence(t *testing.T) {
	cfg := &Config{
		Agent:             types.AgentClaude,
		AgentPathOverride: map[string]string{"codex": "/opt/codex"},
		AgentArgsOverride: map[string][]string{"codex": {"-m", "gpt-5"}},
	}

	// Falls back to agent_path_override / agent_args_override keyed by name.
	fallback := ReviewerSpec{Agent: types.AgentCodex}
	if got := cfg.ReviewerPath(fallback); got != "/opt/codex" {
		t.Errorf("ReviewerPath fallback = %q, want /opt/codex", got)
	}
	if got := cfg.ReviewerArgs(fallback); !reflect.DeepEqual(got, []string{"-m", "gpt-5"}) {
		t.Errorf("ReviewerArgs fallback = %v, want [-m gpt-5]", got)
	}

	// Per-spec Path/Args take precedence.
	override := ReviewerSpec{Agent: types.AgentCodex, Path: "/custom/codex", Args: []string{"-m", "o3"}}
	if got := cfg.ReviewerPath(override); got != "/custom/codex" {
		t.Errorf("ReviewerPath per-spec = %q, want /custom/codex", got)
	}
	if got := cfg.ReviewerArgs(override); !reflect.DeepEqual(got, []string{"-m", "o3"}) {
		t.Errorf("ReviewerArgs per-spec = %v, want [-m o3]", got)
	}

	// Default binary when no override.
	def := ReviewerSpec{Agent: types.AgentPi}
	if got := cfg.ReviewerPath(def); got != "pi" {
		t.Errorf("ReviewerPath default = %q, want pi", got)
	}
}
