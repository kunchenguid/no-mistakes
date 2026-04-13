package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

func TestLoadGlobal_Defaults(t *testing.T) {
	// Non-existent file should return defaults
	cfg, err := LoadGlobal("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentClaude {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentClaude)
	}
	if cfg.CITimeout != 4*time.Hour {
		t.Errorf("ci_timeout = %v, want %v", cfg.CITimeout, 4*time.Hour)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log_level = %q, want %q", cfg.LogLevel, "info")
	}
	if len(cfg.AgentPathOverride) != 0 {
		t.Errorf("agent_path_override = %v, want empty", cfg.AgentPathOverride)
	}
}

func TestEnsureDefaultGlobalConfig_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	EnsureDefaultGlobalConfig(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config file not created: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"agent: claude",
		"ci_timeout:",
		"log_level: info",
		"# agent_path_override:",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("default config missing %q", want)
		}
	}
}

func TestEnsureDefaultGlobalConfig_CreatedConfigIsLoadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	EnsureDefaultGlobalConfig(path)

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error on reload: %v", err)
	}
	if cfg.Agent != types.AgentClaude {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentClaude)
	}
	if cfg.CITimeout != 4*time.Hour {
		t.Errorf("ci_timeout = %v, want %v", cfg.CITimeout, 4*time.Hour)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log_level = %q, want %q", cfg.LogLevel, "info")
	}
}

func TestEnsureDefaultGlobalConfig_DoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	custom := "agent: codex\nlog_level: debug\n"
	if err := os.WriteFile(path, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}

	EnsureDefaultGlobalConfig(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read config: %v", err)
	}
	if string(data) != custom {
		t.Errorf("config was overwritten:\ngot:  %q\nwant: %q", string(data), custom)
	}
}

func TestEnsureDefaultGlobalConfig_SkipsOnStatPermissionError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("agent: codex\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Skip("cannot restrict directory permissions")
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	EnsureDefaultGlobalConfig(path)

	os.Chmod(dir, 0o755)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read config: %v", err)
	}
	if string(data) != "agent: codex\n" {
		t.Errorf("config was overwritten despite stat permission error:\ngot:  %q\nwant: %q", string(data), "agent: codex\n")
	}
}

func TestEnsureDefaultGlobalConfig_CreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "config.yaml")

	EnsureDefaultGlobalConfig(path)

	if _, err := os.Stat(path); err != nil {
		t.Errorf("config file not created in nested dir: %v", err)
	}
}

func TestLoadGlobal_DoesNotCreateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	_, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(path); err == nil {
		t.Error("LoadGlobal should not create config file")
	}
}

func TestLoadGlobal_FromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `agent: codex
agent_path_override:
  claude: /usr/local/bin/claude
  codex: /opt/codex
ci_timeout: "2h30m"
log_level: "debug"
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentCodex {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentCodex)
	}
	if cfg.CITimeout != 2*time.Hour+30*time.Minute {
		t.Errorf("ci_timeout = %v, want %v", cfg.CITimeout, 2*time.Hour+30*time.Minute)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("log_level = %q, want %q", cfg.LogLevel, "debug")
	}
	if cfg.AgentPathOverride["claude"] != "/usr/local/bin/claude" {
		t.Errorf("claude path = %q, want %q", cfg.AgentPathOverride["claude"], "/usr/local/bin/claude")
	}
	if cfg.AgentPathOverride["codex"] != "/opt/codex" {
		t.Errorf("codex path = %q, want %q", cfg.AgentPathOverride["codex"], "/opt/codex")
	}
}

func TestLoadGlobal_PartialOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// Only override agent, rest should be defaults
	data := `agent: opencode
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentOpenCode {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentOpenCode)
	}
	if cfg.CITimeout != 4*time.Hour {
		t.Errorf("ci_timeout = %v, want %v (should be default)", cfg.CITimeout, 4*time.Hour)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log_level = %q, want %q (should be default)", cfg.LogLevel, "info")
	}
}

func TestLoadGlobal_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("{{invalid"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestLoadGlobal_InvalidDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`ci_timeout: "not-a-duration"`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestLoadGlobal_LegacyBabysitTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`babysit_timeout: "90m"`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CITimeout != 90*time.Minute {
		t.Fatalf("ci_timeout = %v, want %v", cfg.CITimeout, 90*time.Minute)
	}
}

func TestLoadGlobal_LegacyAutoFixBabysit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("auto_fix:\n  babysit: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
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

func TestMerge_GlobalOnly(t *testing.T) {
	global := &GlobalConfig{
		Agent:     types.AgentClaude,
		CITimeout: 4 * time.Hour,
		LogLevel:  "info",
	}
	repo := &RepoConfig{}

	cfg := Merge(global, repo)
	if cfg.Agent != types.AgentClaude {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentClaude)
	}
	if cfg.CITimeout != 4*time.Hour {
		t.Errorf("ci_timeout = %v", cfg.CITimeout)
	}
}

func TestMerge_RepoOverridesAgent(t *testing.T) {
	global := &GlobalConfig{
		Agent:             types.AgentClaude,
		AgentPathOverride: map[string]string{"claude": "/usr/bin/claude"},
		CITimeout:         4 * time.Hour,
		LogLevel:          "info",
	}
	repo := &RepoConfig{
		Agent: types.AgentCodex,
		Commands: Commands{
			Test: "make test",
		},
	}

	cfg := Merge(global, repo)
	if cfg.Agent != types.AgentCodex {
		t.Errorf("agent = %q, want %q (repo override)", cfg.Agent, types.AgentCodex)
	}
	if cfg.AgentPathOverride["claude"] != "/usr/bin/claude" {
		t.Errorf("agent path override lost during merge")
	}
	if cfg.Commands.Test != "make test" {
		t.Errorf("test = %q", cfg.Commands.Test)
	}
	if cfg.CITimeout != 4*time.Hour {
		t.Errorf("ci_timeout = %v", cfg.CITimeout)
	}
}

func TestMerge_RepoDoesNotOverrideWhenEmpty(t *testing.T) {
	global := &GlobalConfig{
		Agent:     types.AgentRovoDev,
		CITimeout: 2 * time.Hour,
		LogLevel:  "debug",
	}
	repo := &RepoConfig{
		// Agent is empty — should not override
		Commands: Commands{
			Lint: "eslint .",
		},
	}

	cfg := Merge(global, repo)
	if cfg.Agent != types.AgentRovoDev {
		t.Errorf("agent = %q, want %q (empty repo should not override)", cfg.Agent, types.AgentRovoDev)
	}
	if cfg.Commands.Lint != "eslint ." {
		t.Errorf("lint = %q", cfg.Commands.Lint)
	}
}

func TestAgentPath_Override(t *testing.T) {
	cfg := &Config{
		Agent:             types.AgentClaude,
		AgentPathOverride: map[string]string{"claude": "/custom/claude"},
	}
	if got := cfg.AgentPath(); got != "/custom/claude" {
		t.Errorf("AgentPath() = %q, want %q", got, "/custom/claude")
	}
}

func TestAgentPath_DefaultBinaries(t *testing.T) {
	tests := []struct {
		agent types.AgentName
		want  string
	}{
		{types.AgentClaude, "claude"},
		{types.AgentCodex, "codex"},
		{types.AgentRovoDev, "acli"},
		{types.AgentOpenCode, "opencode"},
	}
	for _, tt := range tests {
		cfg := &Config{Agent: tt.agent}
		if got := cfg.AgentPath(); got != tt.want {
			t.Errorf("AgentPath() for %q = %q, want %q", tt.agent, got, tt.want)
		}
	}
}

func TestDefaultConfigYAML_MatchesGoDefaults(t *testing.T) {
	var raw globalConfigRaw
	if err := yaml.Unmarshal([]byte(defaultConfigYAML), &raw); err != nil {
		t.Fatalf("defaultConfigYAML is not valid YAML: %v", err)
	}

	if raw.Agent != types.AgentClaude {
		t.Errorf("YAML agent = %q, Go default = %q", raw.Agent, types.AgentClaude)
	}
	d, err := time.ParseDuration(raw.CITimeout)
	if err != nil {
		t.Fatalf("YAML ci_timeout %q is not a valid duration: %v", raw.CITimeout, err)
	}
	if d != 4*time.Hour {
		t.Errorf("YAML ci_timeout = %v, Go default = %v", d, 4*time.Hour)
	}
	if raw.LogLevel != "info" {
		t.Errorf("YAML log_level = %q, Go default = %q", raw.LogLevel, "info")
	}
	defaults := autoFixDefaults()
	if raw.AutoFix.Lint == nil || *raw.AutoFix.Lint != defaults.Lint {
		t.Errorf("YAML auto_fix.lint = %v, Go default = %d", raw.AutoFix.Lint, defaults.Lint)
	}
	if raw.AutoFix.Test == nil || *raw.AutoFix.Test != defaults.Test {
		t.Errorf("YAML auto_fix.test = %v, Go default = %d", raw.AutoFix.Test, defaults.Test)
	}
	if raw.AutoFix.Review == nil || *raw.AutoFix.Review != defaults.Review {
		t.Errorf("YAML auto_fix.review = %v, Go default = %d", raw.AutoFix.Review, defaults.Review)
	}
	if raw.AutoFix.Document == nil || *raw.AutoFix.Document != defaults.Document {
		t.Errorf("YAML auto_fix.document = %v, Go default = %d", raw.AutoFix.Document, defaults.Document)
	}
	if raw.AutoFix.CI == nil || *raw.AutoFix.CI != defaults.CI {
		t.Errorf("YAML auto_fix.ci = %v, Go default = %d", raw.AutoFix.CI, defaults.CI)
	}
	if raw.AutoFix.Rebase == nil || *raw.AutoFix.Rebase != defaults.Rebase {
		t.Errorf("YAML auto_fix.rebase = %v, Go default = %d", raw.AutoFix.Rebase, defaults.Rebase)
	}
}

func TestLoadGlobal_AutoFixDefaults(t *testing.T) {
	cfg, err := LoadGlobal("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// AutoFix should be nil (unset) in GlobalConfig
	if cfg.AutoFix.Lint != nil || cfg.AutoFix.Test != nil || cfg.AutoFix.Review != nil ||
		cfg.AutoFix.Document != nil || cfg.AutoFix.CI != nil || cfg.AutoFix.Rebase != nil {
		t.Errorf("expected all AutoFix fields to be nil for defaults, got %+v", cfg.AutoFix)
	}
}

func TestLoadGlobal_AutoFixFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `auto_fix:
  lint: 5
  test: 0
  review: 2
  ci: 1
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AutoFix.Lint == nil || *cfg.AutoFix.Lint != 5 {
		t.Errorf("lint = %v, want 5", cfg.AutoFix.Lint)
	}
	if cfg.AutoFix.Test == nil || *cfg.AutoFix.Test != 0 {
		t.Errorf("test = %v, want 0", cfg.AutoFix.Test)
	}
	if cfg.AutoFix.Review == nil || *cfg.AutoFix.Review != 2 {
		t.Errorf("review = %v, want 2", cfg.AutoFix.Review)
	}
	if cfg.AutoFix.CI == nil || *cfg.AutoFix.CI != 1 {
		t.Errorf("ci =%v, want 1", cfg.AutoFix.CI)
	}
}

func TestLoadGlobal_AutoFixPartial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := `auto_fix:
  lint: 1
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AutoFix.Lint == nil || *cfg.AutoFix.Lint != 1 {
		t.Errorf("lint = %v, want 1", cfg.AutoFix.Lint)
	}
	// Unset fields should remain nil
	if cfg.AutoFix.Test != nil {
		t.Errorf("test = %v, want nil", cfg.AutoFix.Test)
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

func TestMerge_AutoFixDefaults(t *testing.T) {
	global := &GlobalConfig{Agent: types.AgentClaude, CITimeout: 4 * time.Hour, LogLevel: "info"}
	repo := &RepoConfig{}

	cfg := Merge(global, repo)
	if cfg.AutoFix.Lint != 3 {
		t.Errorf("lint = %d, want 3", cfg.AutoFix.Lint)
	}
	if cfg.AutoFix.Test != 3 {
		t.Errorf("test = %d, want 3", cfg.AutoFix.Test)
	}
	if cfg.AutoFix.Review != 3 {
		t.Errorf("review = %d, want 3", cfg.AutoFix.Review)
	}
	if cfg.AutoFix.Document != 3 {
		t.Errorf("document = %d, want 3", cfg.AutoFix.Document)
	}
	if cfg.AutoFix.CI != 3 {
		t.Errorf("ci = %d, want 3", cfg.AutoFix.CI)
	}
	if cfg.AutoFix.Rebase != 0 {
		t.Errorf("rebase = %d, want 0", cfg.AutoFix.Rebase)
	}
}

func TestMerge_AutoFixGlobalOverridesDefaults(t *testing.T) {
	five := 5
	zero := 0
	global := &GlobalConfig{
		Agent:     types.AgentClaude,
		CITimeout: 4 * time.Hour,
		LogLevel:  "info",
		AutoFix:   AutoFixRaw{Lint: &five, CI: &zero},
	}
	repo := &RepoConfig{}

	cfg := Merge(global, repo)
	if cfg.AutoFix.Lint != 5 {
		t.Errorf("lint = %d, want 5 (global override)", cfg.AutoFix.Lint)
	}
	if cfg.AutoFix.Test != 3 {
		t.Errorf("test = %d, want 3 (default)", cfg.AutoFix.Test)
	}
	if cfg.AutoFix.CI != 0 {
		t.Errorf("ci =%d, want 0 (global override)", cfg.AutoFix.CI)
	}
	if cfg.AutoFix.Rebase != 0 {
		t.Errorf("rebase = %d, want 0 (default, no override)", cfg.AutoFix.Rebase)
	}
}

func TestMerge_AutoFixRepoOverridesGlobal(t *testing.T) {
	five := 5
	one := 1
	zero := 0
	global := &GlobalConfig{
		Agent:     types.AgentClaude,
		CITimeout: 4 * time.Hour,
		LogLevel:  "info",
		AutoFix:   AutoFixRaw{Lint: &five},
	}
	repo := &RepoConfig{
		AutoFix: AutoFixRaw{Lint: &one, Review: &zero},
	}

	cfg := Merge(global, repo)
	if cfg.AutoFix.Lint != 1 {
		t.Errorf("lint = %d, want 1 (repo override)", cfg.AutoFix.Lint)
	}
	if cfg.AutoFix.Review != 0 {
		t.Errorf("review = %d, want 0 (repo override)", cfg.AutoFix.Review)
	}
	if cfg.AutoFix.Test != 3 {
		t.Errorf("test = %d, want 3 (default, no override)", cfg.AutoFix.Test)
	}
}

func TestAutoFixLimit(t *testing.T) {
	cfg := &Config{
		AutoFix: AutoFix{Lint: 5, Test: 2, Review: 0, Document: 1, CI: 3, Rebase: 4},
	}
	tests := []struct {
		step types.StepName
		want int
	}{
		{types.StepLint, 5},
		{types.StepTest, 2},
		{types.StepReview, 0},
		{types.StepDocument, 1},
		{types.StepCI, 3},
		{types.StepRebase, 4},
		{types.StepPush, 0},
		{types.StepPR, 0},
	}
	for _, tt := range tests {
		got := cfg.AutoFixLimit(tt.step)
		if got != tt.want {
			t.Errorf("AutoFixLimit(%q) = %d, want %d", tt.step, got, tt.want)
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
		{"DEBUG", slog.LevelInfo}, // case-sensitive, unrecognized defaults to info
	}
	for _, tt := range tests {
		got := ParseLogLevel(tt.input)
		if got != tt.want {
			t.Errorf("ParseLogLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
