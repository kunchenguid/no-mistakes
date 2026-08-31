package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLoadRepo_Defaults(t *testing.T) {
	// Non-existent directory or no .no-mistakes.yaml
	cfg, err := LoadRepo("/nonexistent/dir")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != "" {
		t.Errorf("agent = %q, want empty", cfg.Agent)
	}
	if cfg.Commands.Lint != "" {
		t.Errorf("lint = %q, want empty", cfg.Commands.Lint)
	}
	if cfg.Commands.Test != "" {
		t.Errorf("test = %q, want empty", cfg.Commands.Test)
	}
	if cfg.Commands.Format != "" {
		t.Errorf("format = %q, want empty", cfg.Commands.Format)
	}
	if len(cfg.IgnorePatterns) != 0 {
		t.Errorf("ignore_patterns = %v, want empty", cfg.IgnorePatterns)
	}
}

func TestLoadRepo_FromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".no-mistakes.yaml")
	data := `agent: codex
commands:
  lint: "golangci-lint run ./..."
  test: "go test -race ./..."
  format: "gofmt -w ."
ignore_patterns:
  - "*.generated.go"
  - "vendor/**"
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentCodex {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentCodex)
	}
	if cfg.Commands.Lint != "golangci-lint run ./..." {
		t.Errorf("lint = %q", cfg.Commands.Lint)
	}
	if cfg.Commands.Test != "go test -race ./..." {
		t.Errorf("test = %q", cfg.Commands.Test)
	}
	if cfg.Commands.Format != "gofmt -w ." {
		t.Errorf("format = %q", cfg.Commands.Format)
	}
	if len(cfg.IgnorePatterns) != 2 {
		t.Fatalf("ignore_patterns len = %d, want 2", len(cfg.IgnorePatterns))
	}
	if cfg.IgnorePatterns[0] != "*.generated.go" {
		t.Errorf("ignore_patterns[0] = %q", cfg.IgnorePatterns[0])
	}
	if cfg.IgnorePatterns[1] != "vendor/**" {
		t.Errorf("ignore_patterns[1] = %q", cfg.IgnorePatterns[1])
	}
}

// TestLoadRepo_BranchSyncRemoteTimeoutIsNotARepoSetting proves
// branch_sync_remote_timeout is inert in .no-mistakes.yaml: RepoConfig has
// no matching field, so a pushed branch cannot widen or narrow how long
// guarded branch synchronization waits before failing closed. It is a
// global-only operator machine setting (config.GlobalConfig,
// DefaultBranchSyncRemoteTimeout).
func TestLoadRepo_BranchSyncRemoteTimeoutIsNotARepoSetting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".no-mistakes.yaml")
	data := `branch_sync_remote_timeout: "999s"`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != "" || cfg.Commands.Test != "" || cfg.Commands.Lint != "" || cfg.Commands.Format != "" {
		t.Fatalf("unrelated repo config fields changed: %#v", cfg)
	}
}

// TestLoadRepo_AgentTimeoutIsNotARepoSetting proves agent_timeout is inert in
// .no-mistakes.yaml: RepoConfig has no matching field, so a pushed branch
// cannot widen or remove the default per-invocation agent bound. It is a
// global-only operator machine setting (config.GlobalConfig, DefaultAgentTimeout).
func TestLoadRepo_AgentTimeoutIsNotARepoSetting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".no-mistakes.yaml")
	data := `agent_timeout: "999s"`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != "" || cfg.Commands.Test != "" || cfg.Commands.Lint != "" || cfg.Commands.Format != "" {
		t.Fatalf("unrelated repo config fields changed: %#v", cfg)
	}
}

// TestLoadRepo_TestAgentTimeoutIsNotARepoSetting proves test_agent_timeout is
// inert in .no-mistakes.yaml: RepoConfig has no matching field, so a pushed
// branch cannot widen or remove the Test-step evidence-agent bound. It is a
// global-only operator machine setting (config.GlobalConfig,
// DefaultTestAgentTimeout).
func TestLoadRepo_TestAgentTimeoutIsNotARepoSetting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".no-mistakes.yaml")
	data := `test_agent_timeout: "999s"`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != "" || cfg.Commands.Test != "" || cfg.Commands.Lint != "" || cfg.Commands.Format != "" {
		t.Fatalf("unrelated repo config fields changed: %#v", cfg)
	}
}

// TestLoadRepo_GateReconcileTimingsAreNotRepoSettings proves
// gate_reconcile_timeout / gate_reconcile_interval are inert in
// .no-mistakes.yaml: RepoConfig has no matching fields, so a pushed branch
// cannot widen the approval-gate reconcile budget. They are global-only
// operator machine settings.
func TestLoadRepo_GateReconcileTimingsAreNotRepoSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".no-mistakes.yaml")
	data := "gate_reconcile_timeout: \"999s\"\ngate_reconcile_interval: \"999s\"\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != "" || cfg.Commands.Test != "" || cfg.Commands.Lint != "" || cfg.Commands.Format != "" {
		t.Fatalf("unrelated repo config fields changed: %#v", cfg)
	}
}

func TestLoadRepo_AgentAcceptsList(t *testing.T) {
	dir := t.TempDir()
	data := `agent: [codex, claude]
`
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentCodex {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentCodex)
	}
	want := []types.AgentName{types.AgentCodex, types.AgentClaude}
	if len(cfg.Agents) != len(want) {
		t.Fatalf("agents = %v, want %v", cfg.Agents, want)
	}
	for i := range want {
		if cfg.Agents[i] != want[i] {
			t.Fatalf("agents = %v, want %v", cfg.Agents, want)
		}
	}
}

func TestLoadRepo_AgentStringPreservesSingleAgent(t *testing.T) {
	dir := t.TempDir()
	data := `agent: codex
`
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentCodex {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentCodex)
	}
	if len(cfg.Agents) != 1 || cfg.Agents[0] != types.AgentCodex {
		t.Fatalf("agents = %v, want [codex]", cfg.Agents)
	}
}

func TestLoadRepo_PartialCommands(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".no-mistakes.yaml")
	data := `commands:
  test: "make test"
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Commands.Test != "make test" {
		t.Errorf("test = %q, want %q", cfg.Commands.Test, "make test")
	}
	if cfg.Commands.Lint != "" {
		t.Errorf("lint = %q, want empty", cfg.Commands.Lint)
	}
	if cfg.Commands.Format != "" {
		t.Errorf("format = %q, want empty", cfg.Commands.Format)
	}
}

func TestLoadRepo_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".no-mistakes.yaml")
	if err := os.WriteFile(path, []byte("{{invalid"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadRepo(dir)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoadRepo_AutoFixFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".no-mistakes.yaml")
	data := `auto_fix:
  review: 0
  ci: 2
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AutoFix.Review == nil || *cfg.AutoFix.Review != 0 {
		t.Errorf("review = %v, want 0", cfg.AutoFix.Review)
	}
	if cfg.AutoFix.CI == nil || *cfg.AutoFix.CI != 2 {
		t.Errorf("ci =%v, want 2", cfg.AutoFix.CI)
	}
}

func TestMerge_CIRerunTransientDefaultsToOnePerCheck(t *testing.T) {
	cfg := Merge(DefaultGlobalConfig(), &RepoConfig{})
	if cfg.CI.RerunTransient != DefaultCIRerunTransient {
		t.Fatalf("ci.rerun_transient = %d, want %d", cfg.CI.RerunTransient, DefaultCIRerunTransient)
	}
}

func TestMerge_CIRerunTransientFromRepoConfig(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want int
	}{
		{"explicit budget", "ci:\n  rerun_transient: 2\n", 2},
		{"zero disables reruns", "ci:\n  rerun_transient: 0\n", 0},
		{"negative disables reruns", "ci:\n  rerun_transient: -3\n", 0},
		{"above cap is capped", "ci:\n  rerun_transient: 99\n", MaxCIRerunTransient},
		{"unset keeps the default", "ci: {}\n", DefaultCIRerunTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			repo, err := LoadRepo(dir)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			cfg := Merge(DefaultGlobalConfig(), repo)
			if cfg.CI.RerunTransient != tc.want {
				t.Fatalf("ci.rerun_transient = %d, want %d", cfg.CI.RerunTransient, tc.want)
			}
		})
	}
}

// The budget is trusted-only per repository, so contributing to a project whose
// default branch this operator does not control would otherwise leave them no
// way to decline spending someone else's CI minutes. The global value is that
// switch, and a repository that states its own preference still overrides it.
func TestMerge_CIRerunTransientGlobalOverrideYieldsToTrustedRepoValue(t *testing.T) {
	globalOff := 0
	global := DefaultGlobalConfig()
	global.CI.RerunTransient = &globalOff

	if got := Merge(global, &RepoConfig{}).CI.RerunTransient; got != globalOff {
		t.Fatalf("ci.rerun_transient with no repo value = %d, want the global %d", got, globalOff)
	}

	repoBudget := 2
	repo := &RepoConfig{}
	repo.CI.RerunTransient = &repoBudget
	if got := Merge(global, repo).CI.RerunTransient; got != repoBudget {
		t.Fatalf("ci.rerun_transient = %d, want the trusted repo value %d", got, repoBudget)
	}
}

func TestLoadGlobal_CIRerunTransientFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("ci:\n  rerun_transient: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CI.RerunTransient == nil {
		t.Fatal("global ci.rerun_transient override was not loaded")
	}
	if *cfg.CI.RerunTransient != 0 {
		t.Fatalf("global ci.rerun_transient = %d, want 0", *cfg.CI.RerunTransient)
	}
}

// Every rerun ci.rerun_transient authorizes is another provider-side workflow
// run billed to the repository, so the budget is read only from the trusted
// default-branch copy: a pushed branch must not be able to raise its own.
func TestEffectiveRepoConfig_CIRerunTransientTrustedOnly(t *testing.T) {
	pushedBudget := MaxCIRerunTransient
	trustedBudget := 0
	pushed := &RepoConfig{}
	pushed.CI.RerunTransient = &pushedBudget
	trusted := &RepoConfig{}
	trusted.CI.RerunTransient = &trustedBudget

	effective := EffectiveRepoConfig(pushed, trusted, false)
	if effective.CI.RerunTransient == nil || *effective.CI.RerunTransient != trustedBudget {
		t.Fatalf("ci.rerun_transient = %v, want %d from the trusted copy", effective.CI.RerunTransient, trustedBudget)
	}
	if got := Merge(DefaultGlobalConfig(), effective).CI.RerunTransient; got != trustedBudget {
		t.Fatalf("resolved ci.rerun_transient = %d, want %d", got, trustedBudget)
	}

	// allow_repo_commands opts in to pushed commands and agent selection, never
	// to spending the maintainer's CI minutes.
	optedIn := EffectiveRepoConfig(pushed, trusted, true)
	if optedIn.CI.RerunTransient == nil || *optedIn.CI.RerunTransient != trustedBudget {
		t.Fatalf("ci.rerun_transient with allow_repo_commands = %v, want %d", optedIn.CI.RerunTransient, trustedBudget)
	}

	// No trusted copy means no repository-set budget at all, not the pushed one.
	withoutTrusted := EffectiveRepoConfig(pushed, nil, false)
	if withoutTrusted.CI.RerunTransient != nil {
		t.Fatalf("ci.rerun_transient without a trusted copy = %v, want unset", withoutTrusted.CI.RerunTransient)
	}
	if got := Merge(DefaultGlobalConfig(), withoutTrusted).CI.RerunTransient; got != DefaultCIRerunTransient {
		t.Fatalf("resolved ci.rerun_transient without a trusted copy = %d, want the built-in default %d", got, DefaultCIRerunTransient)
	}
}

func TestLoadRepo_ReviewPathInstructions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".no-mistakes.yaml")
	data := `review:
  path_instructions:
    - path: "internal/scm/**"
      instructions: |
        Credential-carrying URLs must go through internal/safeurl.
    - path: "docs/**"
      instructions: "Prose changes only. Do not request test coverage."
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Review.PathInstructions) != 2 {
		t.Fatalf("path_instructions len = %d, want 2", len(cfg.Review.PathInstructions))
	}
	if cfg.Review.PathInstructions[0].Path != "internal/scm/**" {
		t.Errorf("path_instructions[0].path = %q", cfg.Review.PathInstructions[0].Path)
	}
	if !strings.Contains(cfg.Review.PathInstructions[0].Instructions, "internal/safeurl") {
		t.Errorf("path_instructions[0].instructions = %q", cfg.Review.PathInstructions[0].Instructions)
	}
	if cfg.Review.PathInstructions[1].Path != "docs/**" {
		t.Errorf("path_instructions[1].path = %q", cfg.Review.PathInstructions[1].Path)
	}
	if cfg.Review.PathInstructions[1].Instructions != "Prose changes only. Do not request test coverage." {
		t.Errorf("path_instructions[1].instructions = %q", cfg.Review.PathInstructions[1].Instructions)
	}
}

func TestLoadRepo_ReviewPathInstructionsDefaultsEmpty(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("commands:\n  lint: \"make lint\"\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Review.PathInstructions) != 0 {
		t.Errorf("path_instructions = %v, want empty", cfg.Review.PathInstructions)
	}
}

// TestParseRepoConfig_ReviewPathInstructionsFailClosed proves a malformed or
// over-cap list is rejected when the config is parsed, so the run aborts before
// an agent starts instead of silently dropping guidance or overrunning the
// review prompt budget.
func TestParseRepoConfig_ReviewPathInstructionsFailClosed(t *testing.T) {
	oversized := "review:\n  path_instructions:\n" +
		"    - path: \"internal/**\"\n      instructions: \"" + strings.Repeat("x", MaxReviewPathInstructionsBytes) + "\"\n"

	tooMany := "review:\n  path_instructions:\n"
	for i := 0; i <= MaxReviewPathInstructions; i++ {
		tooMany += fmt.Sprintf("    - path: \"pkg%d/**\"\n      instructions: \"check %d\"\n", i, i)
	}

	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "missing_path",
			yaml:    "review:\n  path_instructions:\n    - instructions: \"check this\"\n",
			wantErr: "review.path_instructions[0].path must not be empty",
		},
		{
			name:    "blank_path",
			yaml:    "review:\n  path_instructions:\n    - path: \"   \"\n      instructions: \"check this\"\n",
			wantErr: "review.path_instructions[0].path must not be empty",
		},
		{
			name:    "missing_instructions",
			yaml:    "review:\n  path_instructions:\n    - path: \"internal/**\"\n",
			wantErr: "review.path_instructions[0].instructions must not be empty",
		},
		// A value made only of merge-conflict markers renders as an empty block,
		// so it must be rejected here instead of disappearing from the prompt.
		{
			name:    "instructions_render_empty",
			yaml:    "review:\n  path_instructions:\n    - path: \"internal/**\"\n      instructions: \"=======\"\n",
			wantErr: "is left empty once merge-conflict markers are removed",
		},
		{
			name:    "instructions_render_empty_multiple_markers",
			yaml:    "review:\n  path_instructions:\n    - path: \"internal/**\"\n      instructions: \" <<<<<<<  >>>>>>> \"\n",
			wantErr: "is left empty once merge-conflict markers are removed",
		},
		{
			name:    "bad_glob",
			yaml:    "review:\n  path_instructions:\n    - path: \"internal/[a-\"\n      instructions: \"check this\"\n",
			wantErr: "is not a valid glob",
		},
		{
			name:    "bare_subtree_pattern",
			yaml:    "review:\n  path_instructions:\n    - path: \"/**\"\n      instructions: \"check this\"\n",
			wantErr: "subtree pattern needs a directory before /**",
		},
		{
			name:    "too_many_entries",
			yaml:    tooMany,
			wantErr: fmt.Sprintf("at most %d are allowed", MaxReviewPathInstructions),
		},
		{
			name:    "over_byte_budget",
			yaml:    oversized,
			wantErr: "so the prompt stays within budget",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadRepoFromBytes([]byte(c.yaml))
			if err == nil {
				t.Fatalf("expected an error for %s", c.name)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), c.wantErr)
			}
		})
	}
}

// Instruction text that merely mentions a conflict marker still carries content,
// so it must parse rather than be rejected with the empty-render error.
func TestParseRepoConfig_ReviewPathInstructionsKeepsTextAroundMarkers(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("review:\n  path_instructions:\n    - path: \"internal/**\"\n      instructions: \"Never leave <<<<<<< markers behind.\"\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Review.PathInstructions) != 1 {
		t.Fatalf("path_instructions len = %d, want 1", len(cfg.Review.PathInstructions))
	}
	if got := RenderedInstructions(cfg.Review.PathInstructions[0].Instructions); got != "Never leave   markers behind." {
		t.Fatalf("rendered instructions = %q; conflict markers are stripped from prompt text, so the docs must say so", got)
	}
}

// TestParseRepoConfig_ReviewPathInstructionsAtCapsIsValid pins both boundaries:
// a list exactly at the entry cap and a section exactly at the byte cap parse,
// and one byte more fails, so the limits reject only what exceeds them.
func TestParseRepoConfig_ReviewPathInstructionsAtCapsIsValid(t *testing.T) {
	atEntryCap := "review:\n  path_instructions:\n"
	for i := 0; i < MaxReviewPathInstructions; i++ {
		atEntryCap += fmt.Sprintf("    - path: \"pkg%d/**\"\n      instructions: \"check %d\"\n", i, i)
	}
	cfg, err := LoadRepoFromBytes([]byte(atEntryCap))
	if err != nil {
		t.Fatalf("unexpected error at the entry cap: %v", err)
	}
	if len(cfg.Review.PathInstructions) != MaxReviewPathInstructions {
		t.Fatalf("path_instructions len = %d, want %d", len(cfg.Review.PathInstructions), MaxReviewPathInstructions)
	}

	// Size one entry so the accounted section lands exactly on the cap.
	path := "internal/**"
	frame := ReviewPathInstructionsBytes([]PathInstruction{{Path: path, Instructions: ""}})
	body := strings.Repeat("x", MaxReviewPathInstructionsBytes-frame)
	entries := []PathInstruction{{Path: path, Instructions: body}}
	if got := ReviewPathInstructionsBytes(entries); got != MaxReviewPathInstructionsBytes {
		t.Fatalf("accounted bytes = %d, want exactly the cap %d", got, MaxReviewPathInstructionsBytes)
	}
	yamlFor := func(instructions string) []byte {
		return []byte("review:\n  path_instructions:\n    - path: \"" + path + "\"\n      instructions: \"" + instructions + "\"\n")
	}
	if _, err := LoadRepoFromBytes(yamlFor(body)); err != nil {
		t.Fatalf("unexpected error exactly at the byte cap: %v", err)
	}
	if _, err := LoadRepoFromBytes(yamlFor(body + "x")); err == nil {
		t.Fatal("expected one byte over the cap to fail")
	}
}

// The accounting must charge for everything the review step injects: the
// heading, every block label, the separators, the path, the instructions, and
// the matched-file allowance. internal/pipeline/steps owns the drift check
// against the real rendered section.
func TestReviewPathInstructionsBytes_CountsTheWholeSection(t *testing.T) {
	if got := ReviewPathInstructionsBytes(nil); got != 0 {
		t.Fatalf("ReviewPathInstructionsBytes(nil) = %d, want 0", got)
	}

	one := []PathInstruction{{Path: "a/**", Instructions: "check it"}}
	wantOne := len("\n\n") + len(ReviewPathInstructionsHeading) + len("\n") +
		len(ReviewPathInstructionsPathLabel) + len("a/**") + len("\n") +
		len(ReviewPathInstructionsFilesLabel) + ReviewPathInstructionsMaxFilesBytes + len("\n") +
		len(ReviewPathInstructionsRulesLabel) + len("\n") +
		len("check it")
	if got := ReviewPathInstructionsBytes(one); got != wantOne {
		t.Fatalf("ReviewPathInstructionsBytes(one entry) = %d, want %d", got, wantOne)
	}

	two := append(append([]PathInstruction{}, one...), PathInstruction{Path: "b/**", Instructions: "check it too"})
	wantTwo := wantOne + len("\n\n") +
		len(ReviewPathInstructionsPathLabel) + len("b/**") + len("\n") +
		len(ReviewPathInstructionsFilesLabel) + ReviewPathInstructionsMaxFilesBytes + len("\n") +
		len(ReviewPathInstructionsRulesLabel) + len("\n") +
		len("check it too")
	if got := ReviewPathInstructionsBytes(two); got != wantTwo {
		t.Fatalf("ReviewPathInstructionsBytes(two entries) = %d, want %d", got, wantTwo)
	}

	// Surrounding whitespace is trimmed before rendering, so it is not charged.
	padded := []PathInstruction{{Path: "  a/**  ", Instructions: "  check it  "}}
	if got := ReviewPathInstructionsBytes(padded); got != wantOne {
		t.Fatalf("ReviewPathInstructionsBytes(padded) = %d, want %d", got, wantOne)
	}
}

func TestLoadRepo_LegacyAutoFixBabysit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".no-mistakes.yaml")
	if err := os.WriteFile(path, []byte("auto_fix:\n  babysit: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AutoFix.CI == nil {
		t.Fatal("ci auto-fix override was not loaded")
	}
	if *cfg.AutoFix.CI != 0 {
		t.Fatalf("ci auto-fix = %d, want 0", *cfg.AutoFix.CI)
	}
}
