package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

// GlobalConfig represents ~/.no-mistakes/config.yaml.
type GlobalConfig struct {
	Agent             types.AgentName   `yaml:"agent"`
	AgentPathOverride map[string]string `yaml:"agent_path_override"`
	BabysitTimeout    time.Duration     `yaml:"-"`
	LogLevel          string            `yaml:"log_level"`
}

// globalConfigRaw is the on-disk YAML representation with duration as string.
type globalConfigRaw struct {
	Agent             types.AgentName   `yaml:"agent"`
	AgentPathOverride map[string]string `yaml:"agent_path_override"`
	BabysitTimeout    string            `yaml:"babysit_timeout"`
	LogLevel          string            `yaml:"log_level"`
}

// RepoConfig represents .no-mistakes.yaml in a repo root.
type RepoConfig struct {
	Agent          types.AgentName `yaml:"agent"`
	Commands       Commands        `yaml:"commands"`
	IgnorePatterns []string        `yaml:"ignore_patterns"`
}

// Commands holds optional per-repo command overrides.
type Commands struct {
	Lint   string `yaml:"lint"`
	Test   string `yaml:"test"`
	Format string `yaml:"format"`
}

// Config is the merged result of global + per-repo configuration.
type Config struct {
	Agent             types.AgentName
	AgentPathOverride map[string]string
	BabysitTimeout    time.Duration
	LogLevel          string
	Commands          Commands
	IgnorePatterns    []string
}

// defaultConfigYAML is the template written when no global config file exists.
const defaultConfigYAML = `# no-mistakes global configuration

# Agent to use for code generation
# Options: claude, codex, rovodev, opencode
agent: claude

# Maximum time to babysit a run before timing out
babysit_timeout: "4h"

# Log level for daemon output
# Options: debug, info, warn, error
log_level: info

# Override agent binary paths (optional)
# agent_path_override:
#   claude: /usr/local/bin/claude
#   codex: /opt/codex
`

// defaultBinary maps agent names to their default binary names.
var defaultBinary = map[types.AgentName]string{
	types.AgentClaude:   "claude",
	types.AgentCodex:    "codex",
	types.AgentRovoDev:  "acli",
	types.AgentOpenCode: "opencode",
}

// AgentPath returns the binary path for the configured agent,
// using agent_path_override if set, otherwise the default binary name.
func (c *Config) AgentPath() string {
	if c.AgentPathOverride != nil {
		if p, ok := c.AgentPathOverride[string(c.Agent)]; ok {
			return p
		}
	}
	if b, ok := defaultBinary[c.Agent]; ok {
		return b
	}
	return string(c.Agent)
}

// EnsureDefaultGlobalConfig writes the default config file at path if it does
// not already exist. Failures are logged at debug level and silently ignored.
func EnsureDefaultGlobalConfig(path string) {
	if _, err := os.Stat(path); err == nil {
		return
	} else if !errors.Is(err, fs.ErrNotExist) {
		slog.Debug("failed to stat config path", "path", path, "error", err)
		return
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		slog.Debug("failed to create config directory", "path", filepath.Dir(path), "error", mkErr)
		return
	}
	if wErr := os.WriteFile(path, []byte(defaultConfigYAML), 0o644); wErr != nil {
		slog.Debug("failed to write default config", "path", path, "error", wErr)
	}
}

// LoadGlobal reads global config from path. Returns defaults if file doesn't exist.
func LoadGlobal(path string) (*GlobalConfig, error) {
	cfg := &GlobalConfig{
		Agent:          types.AgentClaude,
		BabysitTimeout: 4 * time.Hour,
		LogLevel:       "info",
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read global config: %w", err)
	}

	var raw globalConfigRaw
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse global config: %w", err)
	}

	if raw.Agent != "" {
		cfg.Agent = raw.Agent
	}
	if raw.AgentPathOverride != nil {
		cfg.AgentPathOverride = raw.AgentPathOverride
	}
	if raw.BabysitTimeout != "" {
		d, err := time.ParseDuration(raw.BabysitTimeout)
		if err != nil {
			return nil, fmt.Errorf("parse babysit_timeout %q: %w", raw.BabysitTimeout, err)
		}
		cfg.BabysitTimeout = d
	}
	if raw.LogLevel != "" {
		cfg.LogLevel = raw.LogLevel
	}

	return cfg, nil
}

// LoadRepo reads per-repo config from dir/.no-mistakes.yaml.
// Returns zero-value config if file doesn't exist.
func LoadRepo(dir string) (*RepoConfig, error) {
	cfg := &RepoConfig{}

	path := filepath.Join(dir, ".no-mistakes.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read repo config: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse repo config: %w", err)
	}

	return cfg, nil
}

// ParseLogLevel converts a log level string to slog.Level.
// Accepted values: "debug", "info", "warn", "error". Defaults to slog.LevelInfo.
func ParseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Merge combines global and per-repo config. Per-repo agent overrides global
// when non-empty. Commands and ignore patterns come from repo config only.
func Merge(global *GlobalConfig, repo *RepoConfig) *Config {
	cfg := &Config{
		Agent:             global.Agent,
		AgentPathOverride: global.AgentPathOverride,
		BabysitTimeout:    global.BabysitTimeout,
		LogLevel:          global.LogLevel,
		Commands:          repo.Commands,
		IgnorePatterns:    repo.IgnorePatterns,
	}

	if repo.Agent != "" {
		cfg.Agent = repo.Agent
	}

	return cfg
}
