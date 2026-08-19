package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const reviewFleetYAML = `review_fleet:
  enabled: true
  reviewers:
    test-adversary:
      model: gpt-5.4
      reasoning_effort: high
    correctness:
      model: gpt-5.4-mini
      reasoning_effort: medium
    architecture:
      model: gpt-5.4
      reasoning_effort: xhigh
    security:
      model: gpt-5.4
      reasoning_effort: high
      high_risk_paths:
        - internal/auth/**
        - internal/crypto/*.go
      escalated_reasoning_effort: max
  consolidator:
    model: gpt-5.4
    reasoning_effort: high
  certifier:
    model: gpt-5.4-mini
    reasoning_effort: medium
`

func TestLoadGlobal_ReviewFleetDefaultsDisabled(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("agent: claude\n"))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	if cfg.ReviewFleet.Enabled {
		t.Fatal("review fleet is enabled by default")
	}
	if len(cfg.ReviewFleet.Reviewers) != 0 {
		t.Fatalf("default reviewers = %#v, want empty", cfg.ReviewFleet.Reviewers)
	}
}

func TestLoadGlobal_ReviewFleetResolvesProfiles(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte(reviewFleetYAML))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	if !cfg.ReviewFleet.Enabled {
		t.Fatal("review fleet is disabled")
	}
	if len(cfg.ReviewFleet.Reviewers) != 4 {
		t.Fatalf("reviewer count = %d, want 4", len(cfg.ReviewFleet.Reviewers))
	}
	security := cfg.ReviewFleet.Reviewers[ReviewFleetRoleSecurity]
	if security.Model != "gpt-5.4" || security.ReasoningEffort != "high" || security.EscalatedReasoningEffort != "max" {
		t.Fatalf("security profile = %#v", security)
	}
	if !reflect.DeepEqual(security.HighRiskPaths, []string{"internal/auth/**", "internal/crypto/*.go"}) {
		t.Fatalf("high risk paths = %#v", security.HighRiskPaths)
	}
	if cfg.ReviewFleet.Consolidator.Model != "gpt-5.4" || cfg.ReviewFleet.Certifier.Model != "gpt-5.4-mini" {
		t.Fatalf("support profiles = %#v / %#v", cfg.ReviewFleet.Consolidator, cfg.ReviewFleet.Certifier)
	}
}

func TestReviewFleetEnabledRequiresExactRolesAndSupportProfiles(t *testing.T) {
	base := `review_fleet:
  enabled: true
  reviewers:
`
	validReviewer := "    test-adversary:\n      model: m\n      reasoning_effort: low\n"
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "missing_role",
			yaml: base + validReviewer +
				"    correctness:\n      model: m\n      reasoning_effort: low\n" +
				"    architecture:\n      model: m\n      reasoning_effort: low\n" +
				"    security:\n      model: m\n      reasoning_effort: low\n" +
				"    extra:\n      model: m\n      reasoning_effort: low\n" +
				"  consolidator:\n    model: m\n    reasoning_effort: low\n" +
				"  certifier:\n    model: m\n    reasoning_effort: low\n",
			want: "exactly the fixed reviewer roles",
		},
		{
			name: "missing_consolidator",
			yaml: reviewFleetReviewersOnlyYAML() +
				"  certifier:\n    model: m\n    reasoning_effort: low\n",
			want: "consolidator",
		},
		{
			name: "missing_certifier",
			yaml: reviewFleetReviewersOnlyYAML() +
				"  consolidator:\n    model: m\n    reasoning_effort: low\n",
			want: "certifier",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadGlobalFromBytes([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func reviewFleetReviewersOnlyYAML() string {
	return `review_fleet:
  enabled: true
  reviewers:
    test-adversary:
      model: m
      reasoning_effort: low
    correctness:
      model: m
      reasoning_effort: low
    architecture:
      model: m
      reasoning_effort: low
    security:
      model: m
      reasoning_effort: low
`
}

func TestReviewFleetValidationBoundsAndEnums(t *testing.T) {
	valid := reviewFleetYAML
	cases := []struct {
		name string
		mut  func(string) string
		want string
	}{
		{
			name: "model_too_long",
			mut: func(yaml string) string {
				return strings.Replace(yaml, "model: gpt-5.4\n", "model: "+strings.Repeat("m", MaxReviewFleetModelBytes+1)+"\n", 1)
			},
			want: "model",
		},
		{
			name: "bad_reasoning_effort",
			mut: func(yaml string) string {
				return strings.Replace(yaml, "reasoning_effort: high", "reasoning_effort: impossible", 1)
			},
			want: "reasoning_effort",
		},
		{
			name: "bad_escalated_reasoning_effort",
			mut: func(yaml string) string {
				return strings.Replace(yaml, "escalated_reasoning_effort: max", "escalated_reasoning_effort: impossible", 1)
			},
			want: "escalated_reasoning_effort",
		},
		{
			name: "escalated_effort_must_be_stronger",
			mut: func(yaml string) string {
				return strings.Replace(yaml, "escalated_reasoning_effort: max", "escalated_reasoning_effort: low", 1)
			},
			want: "stronger",
		},
		{
			name: "paths_without_escalated_effort",
			mut: func(yaml string) string {
				return strings.Replace(yaml, "      escalated_reasoning_effort: max\n", "", 1)
			},
			want: "escalated_reasoning_effort",
		},
		{
			name: "escalated_effort_without_paths",
			mut: func(yaml string) string {
				return strings.Replace(yaml, "      high_risk_paths:\n        - internal/auth/**\n        - internal/crypto/*.go\n", "", 1)
			},
			want: "high_risk_paths",
		},
		{
			name: "bad_glob",
			mut:  func(yaml string) string { return strings.Replace(yaml, "internal/crypto/*.go", "internal/[a-.go", 1) },
			want: "glob",
		},
		{
			name: "bare_subtree_glob",
			mut:  func(yaml string) string { return strings.Replace(yaml, "internal/auth/**", "/**", 1) },
			want: "glob",
		},
		{
			name: "malformed_subtree_glob",
			mut:  func(yaml string) string { return strings.Replace(yaml, "internal/auth/**", "internal/[a-/**", 1) },
			want: "glob",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadGlobalFromBytes([]byte(tc.mut(valid)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	tooMany := strings.Builder{}
	tooMany.WriteString(`review_fleet:
  enabled: true
  reviewers:
`)
	for _, role := range []string{ReviewFleetRoleTestAdversary, ReviewFleetRoleCorrectness, ReviewFleetRoleArchitecture, ReviewFleetRoleSecurity} {
		tooMany.WriteString("    " + role + ":\n      model: m\n      reasoning_effort: low\n")
	}
	tooMany.WriteString("      high_risk_paths:\n")
	for i := 0; i < MaxReviewFleetHighRiskPaths+1; i++ {
		tooMany.WriteString("        - pkg" + string(rune('a'+i%26)) + "/**\n")
	}
	tooMany.WriteString("  consolidator:\n    model: m\n    reasoning_effort: low\n  certifier:\n    model: m\n    reasoning_effort: low\n")
	if _, err := LoadGlobalFromBytes([]byte(tooMany.String())); err == nil || !strings.Contains(err.Error(), "high_risk_paths") {
		t.Fatalf("too many paths error = %v", err)
	}
}

func TestRepoConfigReviewFleetIsNotAnActivationSurface(t *testing.T) {
	repo, err := LoadRepoFromBytes([]byte(reviewFleetYAML))
	if err != nil {
		t.Fatalf("repo config with review_fleet should remain loadable as an ignored global-only key: %v", err)
	}
	global, err := LoadGlobalFromBytes([]byte("agent: " + string(types.AgentCodex) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	merged := Merge(global, repo)
	if merged.ReviewFleet.Enabled {
		t.Fatal("repository review_fleet activated the fleet")
	}
}

func TestReviewFleetCodexArgsAreColdReadOnlyAndPreserveSafeOverrides(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte(`agent_args_override:
  codex:
    - -c
    - service_tier="priority"
` + reviewFleetYAML))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	merged := Merge(cfg, &RepoConfig{})
	args, err := merged.ReviewFleetCodexArgs(ReviewFleetRoleSecurity, false)
	if err != nil {
		t.Fatalf("ReviewFleetCodexArgs: %v", err)
	}
	want := []string{
		"-c", `service_tier="priority"`,
		"-m", "gpt-5.4",
		"-c", `model_reasoning_effort="high"`,
		"--sandbox", "read-only",
		"--ephemeral",
		"-c", "project_doc_max_bytes=0",
		"-c", `shell_environment_policy.inherit="core"`,
		"--ignore-rules",
		"--ignore-user-config",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("fleet args = %#v, want %#v", args, want)
	}
	escalated, err := merged.ReviewFleetCodexArgs(ReviewFleetRoleSecurity, true)
	if err != nil {
		t.Fatalf("escalated ReviewFleetCodexArgs: %v", err)
	}
	if !containsArg(escalated, `model_reasoning_effort="max"`) {
		t.Fatalf("escalated args = %#v, want max effort", escalated)
	}
}

func TestReviewFleetCodexArgsRejectInheritedIsolationFlags(t *testing.T) {
	for _, args := range [][]string{
		{"-m", "gpt-5.4"}, {"--model=gpt-5.4"}, {"-c", `model_reasoning_effort="low"`},
		{"--sandbox", "workspace-write"}, {"--dangerously-bypass-approvals-and-sandbox"},
		{"--resume", "thread"}, {"-c", "project_doc_max_bytes=4096"}, {"-c", "custom_setting=true"}, {"--ignore-rules"},
		{"--add-dir", "/tmp/extra"}, {"--profile", "unsafe"}, {"--enable", "mcp"},
	} {
		name := strings.NewReplacer("-", "dash", "=", "eq", " ", "_").Replace(strings.Join(args, "_"))
		t.Run(name, func(t *testing.T) {
			cfg, err := LoadGlobalFromBytes([]byte(reviewFleetYAML))
			if err != nil {
				t.Fatal(err)
			}
			cfg.AgentArgsOverride = map[string][]string{"codex": args}
			merged := Merge(cfg, &RepoConfig{})
			if _, err := merged.ReviewFleetCodexArgs(ReviewFleetRoleCorrectness, false); err == nil {
				t.Fatalf("expected inherited flags %#v to be rejected", args)
			}
		})
	}
}

func TestLoadGlobal_ReviewFleetRejectsConflictingCodexOverride(t *testing.T) {
	for _, override := range []string{
		"    - -m\n    - gpt-5.4\n",
		"    - -c\n    - model_reasoning_effort=low\n",
		"    - --sandbox\n    - workspace-write\n",
	} {
		yaml := "agent_args_override:\n  codex:\n" + override + reviewFleetYAML
		if _, err := LoadGlobalFromBytes([]byte(yaml)); err == nil || !strings.Contains(err.Error(), "review_fleet") {
			t.Fatalf("override %q error = %v, want review_fleet conflict", override, err)
		}
	}
}

func TestReviewFleetRequiresCodexBinaryWhenEnabled(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte(reviewFleetYAML))
	if err != nil {
		t.Fatal(err)
	}
	merged := Merge(cfg, &RepoConfig{})
	if err := merged.ResolveAgent(nil, func(name string) (string, error) {
		if name == "codex" {
			return "", errNotFoundForTest{}
		}
		return name, nil
	}); err == nil || !strings.Contains(err.Error(), "review_fleet") {
		t.Fatalf("ResolveAgent error = %v, want missing fleet codex binary", err)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

type errNotFoundForTest struct{}

func (errNotFoundForTest) Error() string { return "not found" }
