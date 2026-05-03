package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

// GlobalConfig represents ~/.no-mistakes/config.yaml.
type GlobalConfig struct {
	Agent                types.AgentName     `yaml:"agent"`
	ACPXPath             string              `yaml:"acpx_path"`
	ACPRegistryOverrides map[string]string   `yaml:"acp_registry_overrides"`
	AgentPathOverride    map[string]string   `yaml:"agent_path_override"`
	AgentArgsOverride    map[string][]string `yaml:"agent_args_override"`
	CITimeout            time.Duration       `yaml:"-"`
	LogLevel             string              `yaml:"log_level"`
	AutoFix              AutoFixRaw
}

// globalConfigRaw is the on-disk YAML representation with duration as string.
type globalConfigRaw struct {
	Agent                types.AgentName     `yaml:"agent"`
	ACPXPath             string              `yaml:"acpx_path"`
	ACPRegistryOverrides map[string]string   `yaml:"acp_registry_overrides"`
	AgentPathOverride    map[string]string   `yaml:"agent_path_override"`
	AgentArgsOverride    map[string][]string `yaml:"agent_args_override"`
	CITimeout            string              `yaml:"ci_timeout"`
	BabysitTimeout       string              `yaml:"babysit_timeout"`
	LogLevel             string              `yaml:"log_level"`
	AutoFix              AutoFixRaw          `yaml:"auto_fix"`
}

// RepoConfig represents .no-mistakes.yaml in a repo root.
type RepoConfig struct {
	Agent          types.AgentName `yaml:"agent"`
	Commands       Commands        `yaml:"commands"`
	IgnorePatterns []string        `yaml:"ignore_patterns"`
	AutoFix        AutoFixRaw      `yaml:"auto_fix"`
}

// Commands holds optional per-repo command overrides.
type Commands struct {
	Lint   string `yaml:"lint"`
	Test   string `yaml:"test"`
	Format string `yaml:"format"`
}

// AutoFixRaw is the YAML representation of auto-fix config.
// Pointer fields distinguish "not set" (nil) from "set to 0" (disabled).
type AutoFixRaw struct {
	Lint     *int `yaml:"lint"`
	Test     *int `yaml:"test"`
	Review   *int `yaml:"review"`
	Document *int `yaml:"document"`
	CI       *int `yaml:"ci"`
	Babysit  *int `yaml:"babysit"`
	Rebase   *int `yaml:"rebase"`
}

// AutoFix holds resolved per-step auto-fix attempt limits.
// A value of 0 means auto-fix is disabled (requires manual approval).
type AutoFix struct {
	Lint     int
	Test     int
	Review   int
	Document int
	CI       int
	Rebase   int
}

// Config is the merged result of global + per-repo configuration.
type Config struct {
	Agent                types.AgentName
	ACPXPath             string
	ACPRegistryOverrides map[string]string
	AgentPathOverride    map[string]string
	AgentArgsOverride    map[string][]string
	CITimeout            time.Duration
	LogLevel             string
	Commands             Commands
	IgnorePatterns       []string
	AutoFix              AutoFix
}

// defaultConfigYAML is the template written when no global config file exists.
const defaultConfigYAML = `# no-mistakes global configuration

# Agent to use for code generation
# Options: auto, claude, codex, rovodev, opencode, pi, acp:<target>
# "auto" detects the first available agent on your system
# Use acp:<target> to run an optional user-installed acpx target, for example acp:gemini
agent: auto

# Optional path to the user-installed acpx binary for acp:<target> agents
# acpx_path: acpx

# Optional ACP target command overrides for acp:<target> agents
# acp_registry_overrides:
#   local-gemini: node /opt/mock-acp-agent.mjs

# Maximum time to monitor CI before timing out
ci_timeout: "4h"

# Log level for daemon output
# Options: debug, info, warn, error
log_level: info

# Override agent binary paths (optional)
# agent_path_override:
#   claude: /usr/local/bin/claude
#   codex: /opt/codex

# Extra agent CLI flags (optional, global only)
# agent_args_override:
#   codex:
#     - -m
#     - gpt-5.4
#
# Maximum auto-fix attempts per step (0 = disabled, requires manual approval)
auto_fix:
  rebase: 3
  lint: 3
  test: 3
  review: 0
  document: 3
  ci: 3
`

// defaultBinary maps agent names to their default binary names.
var defaultBinary = map[types.AgentName]string{
	types.AgentClaude:   "claude",
	types.AgentCodex:    "codex",
	types.AgentRovoDev:  "acli",
	types.AgentOpenCode: "opencode",
	types.AgentPi:       "pi",
}

// agentProbeOrder is the priority order for auto-detecting agents.
var agentProbeOrder = []types.AgentName{
	types.AgentClaude,
	types.AgentCodex,
	types.AgentOpenCode,
	types.AgentRovoDev,
	types.AgentPi,
}

func isACPAgent(name types.AgentName) bool {
	value := string(name)
	if !strings.HasPrefix(value, "acp:") {
		return false
	}
	target := strings.TrimPrefix(value, "acp:")
	return target != "" && !strings.ContainsAny(target, " \t\r\n")
}

var probeRovoDevSupport = func(ctx context.Context, bin string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "rovodev", "--help")
	output, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false, fmt.Errorf("probe rovodev support via %q timed out", bin)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		text := strings.ToLower(string(output))
		if strings.Contains(text, "unknown command") ||
			strings.Contains(text, "unknown subcommand") ||
			strings.Contains(text, "unrecognized command") ||
			strings.Contains(text, "no help topic for") {
			return false, nil
		}
		return false, fmt.Errorf("probe rovodev support via %q: %w", bin, err)
	}
	return false, fmt.Errorf("probe rovodev support via %q: %w", bin, err)
}

// ResolveAgent resolves AgentAuto to a concrete agent by probing which binaries
// are available on the system. If agent is already set to a specific value, this
// is a no-op. The lookPath function should behave like exec.LookPath.
func (c *Config) ResolveAgent(ctx context.Context, lookPath func(string) (string, error)) error {
	if c.Agent != types.AgentAuto {
		return nil
	}
	probed := make([]string, 0, len(agentProbeOrder))
	for _, name := range agentProbeOrder {
		bin := string(name)
		if b, ok := defaultBinary[name]; ok {
			bin = b
		}
		if c.AgentPathOverride != nil {
			if p, ok := c.AgentPathOverride[string(name)]; ok {
				bin = p
			}
		}
		probed = append(probed, bin)
		resolvedBin, err := lookPath(bin)
		if err == nil {
			if name == types.AgentRovoDev {
				ok, probeErr := probeRovoDevSupport(ctx, resolvedBin)
				if probeErr != nil {
					return probeErr
				}
				if !ok {
					continue
				}
			}
			c.Agent = name
			return nil
		} else if !errors.Is(err, exec.ErrNotFound) && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("resolve %s agent from %q: %w", name, bin, err)
		}
	}
	return fmt.Errorf("no supported agent found in PATH (looked for: %s); install one or set 'agent' in ~/.no-mistakes/config.yaml", strings.Join(probed, ", "))
}

// AgentPath returns the binary path for the configured agent.
// ACP agents use acpx_path if set, otherwise acpx.
// Native agents use agent_path_override if set, otherwise the default binary name.
func (c *Config) AgentPath() string {
	if isACPAgent(c.Agent) {
		if c.ACPXPath != "" {
			return c.ACPXPath
		}
		return "acpx"
	}
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

// AgentArgs returns extra CLI args for the configured agent, as declared in
// agent_args_override. Returns nil when no override is set for this agent.
func (c *Config) AgentArgs() []string {
	if c.AgentArgsOverride == nil {
		return nil
	}
	return c.AgentArgsOverride[string(c.Agent)]
}

// agentArgsOverrideAgents lists agent names accepted as keys in
// agent_args_override.
var agentArgsOverrideAgents = map[string]bool{
	string(types.AgentClaude):   true,
	string(types.AgentCodex):    true,
	string(types.AgentRovoDev):  true,
	string(types.AgentOpenCode): true,
	string(types.AgentPi):       true,
}

// reservedAgentArgs lists flags that no-mistakes manages internally and that
// users cannot override through agent_args_override. A flag is matched by its
// bare form (e.g. "--color") as well as the "--color=value" form.
var reservedAgentArgs = map[string]map[string]bool{
	string(types.AgentClaude): {
		"-p":              true,
		"--print":         true,
		"--verbose":       true,
		"--output-format": true,
		"--json-schema":   true,
	},
	string(types.AgentCodex): {
		"exec":    true,
		"--json":  true,
		"--color": true,
	},
	string(types.AgentRovoDev): {
		"rovodev":                 true,
		"serve":                   true,
		"--disable-session-token": true,
	},
	string(types.AgentOpenCode): {
		"serve":        true,
		"--hostname":   true,
		"--port":       true,
		"--print-logs": true,
	},
	string(types.AgentPi): {
		"--mode":       true,
		"--no-session": true,
	},
}

// validateAgentArgsOverride ensures each agent key is a known agent name and
// that no reserved flag appears. Empty args are rejected to catch trivially
// broken YAML.
func validateAgentArgsOverride(override map[string][]string) error {
	for name, args := range override {
		if !agentArgsOverrideAgents[name] {
			return fmt.Errorf("invalid agent name in agent_args_override: %q (valid: claude, codex, rovodev, opencode, pi)", name)
		}
		reserved := reservedAgentArgs[name]
		for i, arg := range args {
			if strings.TrimSpace(arg) == "" {
				return fmt.Errorf("invalid agent_args_override.%s[%d]: empty arg", name, i)
			}
			base := arg
			if idx := strings.Index(arg, "="); idx > 0 {
				base = arg[:idx]
			}
			if reserved[base] {
				return fmt.Errorf("invalid agent_args_override.%s[%d]: %q is managed by no-mistakes and cannot be overridden", name, i, arg)
			}
		}
	}
	return nil
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
		Agent:     types.AgentAuto,
		CITimeout: 4 * time.Hour,
		LogLevel:  "info",
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
	if raw.ACPXPath != "" {
		cfg.ACPXPath = raw.ACPXPath
	}
	if raw.ACPRegistryOverrides != nil {
		cfg.ACPRegistryOverrides = raw.ACPRegistryOverrides
	}
	if raw.AgentPathOverride != nil {
		cfg.AgentPathOverride = raw.AgentPathOverride
	}
	if raw.AgentArgsOverride != nil {
		if err := validateAgentArgsOverride(raw.AgentArgsOverride); err != nil {
			return nil, err
		}
		cfg.AgentArgsOverride = raw.AgentArgsOverride
	}
	timeoutValue := raw.CITimeout
	if timeoutValue == "" {
		timeoutValue = raw.BabysitTimeout
	}
	if timeoutValue != "" {
		d, err := time.ParseDuration(timeoutValue)
		if err != nil {
			return nil, fmt.Errorf("parse ci_timeout %q: %w", timeoutValue, err)
		}
		cfg.CITimeout = d
	}
	if raw.LogLevel != "" {
		cfg.LogLevel = raw.LogLevel
	}
	if raw.AutoFix.CI == nil {
		raw.AutoFix.CI = raw.AutoFix.Babysit
	}
	cfg.AutoFix = raw.AutoFix

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
	if cfg.AutoFix.CI == nil {
		cfg.AutoFix.CI = cfg.AutoFix.Babysit
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

// autoFixDefaults returns the default auto-fix configuration.
func autoFixDefaults() AutoFix {
	return AutoFix{
		Lint:     3,
		Test:     3,
		Review:   0,
		Document: 3,
		CI:       3,
		Rebase:   3,
	}
}

// applyAutoFixOverrides applies non-nil raw values onto resolved defaults.
func applyAutoFixOverrides(dst *AutoFix, src *AutoFixRaw) {
	if src.Lint != nil {
		dst.Lint = *src.Lint
	}
	if src.Test != nil {
		dst.Test = *src.Test
	}
	if src.Review != nil {
		dst.Review = *src.Review
	}
	if src.Document != nil {
		dst.Document = *src.Document
	}
	if src.CI != nil {
		dst.CI = *src.CI
	}
	if src.Rebase != nil {
		dst.Rebase = *src.Rebase
	}
}

// AutoFixLimit returns the max auto-fix attempts for a given step.
// Steps without auto-fix support return 0.
func (c *Config) AutoFixLimit(step types.StepName) int {
	switch step {
	case types.StepLint:
		return c.AutoFix.Lint
	case types.StepTest:
		return c.AutoFix.Test
	case types.StepReview:
		return c.AutoFix.Review
	case types.StepDocument:
		return c.AutoFix.Document
	case types.StepCI:
		return c.AutoFix.CI
	case types.StepRebase:
		return c.AutoFix.Rebase
	default:
		return 0
	}
}

// Merge combines global and per-repo config. Per-repo agent overrides global
// when non-empty. Commands and ignore patterns come from repo config only.
func Merge(global *GlobalConfig, repo *RepoConfig) *Config {
	af := autoFixDefaults()
	applyAutoFixOverrides(&af, &global.AutoFix)
	applyAutoFixOverrides(&af, &repo.AutoFix)

	cfg := &Config{
		Agent:                global.Agent,
		ACPXPath:             global.ACPXPath,
		ACPRegistryOverrides: global.ACPRegistryOverrides,
		AgentPathOverride:    global.AgentPathOverride,
		AgentArgsOverride:    global.AgentArgsOverride,
		CITimeout:            global.CITimeout,
		LogLevel:             global.LogLevel,
		Commands:             repo.Commands,
		IgnorePatterns:       repo.IgnorePatterns,
		AutoFix:              af,
	}

	if repo.Agent != "" {
		cfg.Agent = repo.Agent
	}

	return cfg
}
