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
	if cfg.BabysitTimeout != 4*time.Hour {
		t.Errorf("babysit_timeout = %v, want %v", cfg.BabysitTimeout, 4*time.Hour)
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
		"babysit_timeout:",
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
	if cfg.BabysitTimeout != 4*time.Hour {
		t.Errorf("babysit_timeout = %v, want %v", cfg.BabysitTimeout, 4*time.Hour)
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
babysit_timeout: "2h30m"
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
	if cfg.BabysitTimeout != 2*time.Hour+30*time.Minute {
		t.Errorf("babysit_timeout = %v, want %v", cfg.BabysitTimeout, 2*time.Hour+30*time.Minute)
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
	if cfg.BabysitTimeout != 4*time.Hour {
		t.Errorf("babysit_timeout = %v, want %v (should be default)", cfg.BabysitTimeout, 4*time.Hour)
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
	if err := os.WriteFile(path, []byte(`babysit_timeout: "not-a-duration"`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatal("expected error for invalid duration")
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
		Agent:          types.AgentClaude,
		BabysitTimeout: 4 * time.Hour,
		LogLevel:       "info",
	}
	repo := &RepoConfig{}

	cfg := Merge(global, repo)
	if cfg.Agent != types.AgentClaude {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentClaude)
	}
	if cfg.BabysitTimeout != 4*time.Hour {
		t.Errorf("babysit_timeout = %v", cfg.BabysitTimeout)
	}
}

func TestMerge_RepoOverridesAgent(t *testing.T) {
	global := &GlobalConfig{
		Agent:             types.AgentClaude,
		AgentPathOverride: map[string]string{"claude": "/usr/bin/claude"},
		BabysitTimeout:    4 * time.Hour,
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
	if cfg.BabysitTimeout != 4*time.Hour {
		t.Errorf("babysit_timeout = %v", cfg.BabysitTimeout)
	}
}

func TestMerge_RepoDoesNotOverrideWhenEmpty(t *testing.T) {
	global := &GlobalConfig{
		Agent:          types.AgentRovoDev,
		BabysitTimeout: 2 * time.Hour,
		LogLevel:       "debug",
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

func TestAgentPath_Default(t *testing.T) {
	cfg := &Config{
		Agent: types.AgentClaude,
	}
	if got := cfg.AgentPath(); got != "claude" {
		t.Errorf("AgentPath() = %q, want %q (default binary name)", got, "claude")
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
	d, err := time.ParseDuration(raw.BabysitTimeout)
	if err != nil {
		t.Fatalf("YAML babysit_timeout %q is not a valid duration: %v", raw.BabysitTimeout, err)
	}
	if d != 4*time.Hour {
		t.Errorf("YAML babysit_timeout = %v, Go default = %v", d, 4*time.Hour)
	}
	if raw.LogLevel != "info" {
		t.Errorf("YAML log_level = %q, Go default = %q", raw.LogLevel, "info")
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
