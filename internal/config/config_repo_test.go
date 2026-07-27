package config

import (
	"os"
	"path/filepath"
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
