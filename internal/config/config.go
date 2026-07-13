package config

import (
	"bytes"
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
	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"gopkg.in/yaml.v3"
)

// CI monitor timeout constants.
//
// CITimeout is interpreted by the CI step as the maximum time to babysit an
// open PR with no base-branch movement before giving up. The monitor re-arms
// this timer every time the base branch advances (see internal/pipeline/steps
// ci.go), so an actively-rebased PR keeps its monitor. The value is
// deliberately long because a green PR can legitimately wait days on a
// dependency PR or on review; a torn-down or abandoned run is reaped
// explicitly via `no-mistakes axi abort --run <id>` rather than by a short
// timeout.
const (
	// DefaultCITimeout is the monitor's idle timeout when ci_timeout is unset.
	DefaultCITimeout = 7 * 24 * time.Hour
	// DefaultStepQuietWarning is how long a running/fixing step can go without
	// a new log or lifecycle activity before AXI status marks it quiet.
	DefaultStepQuietWarning = 10 * time.Minute
	// DefaultDaemonConnectTimeout bounds client IPC connection attempts to a
	// daemon socket that exists but is not accepting connections.
	DefaultDaemonConnectTimeout = 3 * time.Second
	// CITimeoutUnlimited is the sentinel meaning "monitor until the PR is
	// merged, closed, or the run is aborted - never self-terminate".
	// Any non-positive ci_timeout, or the keywords "unlimited", "none",
	// "off", and "never", resolves to this.
	CITimeoutUnlimited = time.Duration(-1)
)

// GlobalConfig represents ~/.no-mistakes/config.yaml.
type GlobalConfig struct {
	Agent                types.AgentName     `yaml:"agent"`
	Agents               []types.AgentName   `yaml:"-"`
	ACPXPath             string              `yaml:"acpx_path"`
	ACPRegistryOverrides map[string]string   `yaml:"acp_registry_overrides"`
	AgentPathOverride    map[string]string   `yaml:"agent_path_override"`
	AgentArgsOverride    map[string][]string `yaml:"agent_args_override"`
	CITimeout            time.Duration       `yaml:"-"`
	StepQuietWarning     time.Duration       `yaml:"-"`
	DaemonConnectTimeout time.Duration       `yaml:"-"`
	LogLevel             string              `yaml:"log_level"`
	// SessionReuse controls per-run, per-role agent session reuse in the
	// review loop (one durable reviewer session across full reviews, a
	// separate durable fixer session across fix turns). Default true; set
	// session_reuse: false to force every invocation cold.
	SessionReuse bool `yaml:"-"`
	AutoFix      AutoFixRaw
	Intent       IntentRaw
	Test         TestRaw
}

// globalConfigRaw is the on-disk YAML representation with duration as string.
type globalConfigRaw struct {
	Agent                agentList           `yaml:"agent"`
	ACPXPath             string              `yaml:"acpx_path"`
	ACPRegistryOverrides map[string]string   `yaml:"acp_registry_overrides"`
	AgentPathOverride    map[string]string   `yaml:"agent_path_override"`
	AgentArgsOverride    map[string][]string `yaml:"agent_args_override"`
	CITimeout            string              `yaml:"ci_timeout"`
	DaemonConnectTimeout string              `yaml:"daemon_connect_timeout"`
	BabysitTimeout       string              `yaml:"babysit_timeout"`
	StepQuietWarning     string              `yaml:"step_quiet_warning"`
	LogLevel             string              `yaml:"log_level"`
	SessionReuse         *bool               `yaml:"session_reuse"`
	AutoFix              AutoFixRaw          `yaml:"auto_fix"`
	Intent               IntentRaw           `yaml:"intent"`
	Test                 TestRaw             `yaml:"test"`
}

// RepoConfig represents .no-mistakes.yaml in a repo root.
type RepoConfig struct {
	Agent          types.AgentName   `yaml:"agent"`
	Agents         []types.AgentName `yaml:"-"`
	Commands       Commands          `yaml:"commands"`
	IgnorePatterns []string          `yaml:"ignore_patterns"`
	// AllowRepoCommands opts in to honoring the code-executing selection
	// fields (commands.{test,lint,format} and agent) from a contributor's
	// pushed branch instead of the trusted default-branch copy. It is read
	// ONLY from the trusted default-branch copy of .no-mistakes.yaml (never
	// the pushed SHA), so a contributor cannot self-enable. Default false:
	// the pushed branch controls nothing that executes.
	AllowRepoCommands bool       `yaml:"allow_repo_commands"`
	AutoFix           AutoFixRaw `yaml:"auto_fix"`
	Intent            IntentRaw  `yaml:"intent"`
	Test              TestRaw    `yaml:"test"`
	// Document carries the repository's documentation placement policy. It
	// steers the document step's gate prompt, so it is honored ONLY from the
	// trusted default-branch copy of .no-mistakes.yaml (see
	// EffectiveRepoConfig): a contributor's pushed branch must not be able to
	// weaken documentation rules for its own review.
	Document DocumentRaw `yaml:"document"`
	// DisableProjectSettings opts the repository out of loading project-level
	// agent settings/instructions (AGENTS.md/CLAUDE.md and the equivalent
	// per-harness project settings) into gate agents. It exists for
	// agent-orchestration repos (e.g. firstmate) whose project instructions
	// would otherwise install a fleet-captain identity on a gate agent. It is a
	// SECURITY boundary honored ONLY from the trusted default-branch copy of
	// .no-mistakes.yaml (see EffectiveRepoConfig and the daemon's
	// assertGateTrustedConfigReadable): a contributor's pushed branch must not be
	// able to turn it off (or on). Default false; a plain bool so a missing key
	// or a YAML/JSON null is falsy and preserves current loading.
	DisableProjectSettings bool `yaml:"disable_project_settings"`
}

// DocumentRaw is the YAML representation of document-step settings.
type DocumentRaw struct {
	// Instructions augment (never replace) the built-in documentation
	// placement policy with the repository's ownership map or extra
	// placement rules.
	Instructions string `yaml:"instructions"`
}

func (c *RepoConfig) UnmarshalYAML(value *yaml.Node) error {
	type repoConfigRaw struct {
		Agent                  agentList   `yaml:"agent"`
		Commands               Commands    `yaml:"commands"`
		IgnorePatterns         []string    `yaml:"ignore_patterns"`
		AllowRepoCommands      bool        `yaml:"allow_repo_commands"`
		AutoFix                AutoFixRaw  `yaml:"auto_fix"`
		Intent                 IntentRaw   `yaml:"intent"`
		Test                   TestRaw     `yaml:"test"`
		Document               DocumentRaw `yaml:"document"`
		DisableProjectSettings bool        `yaml:"disable_project_settings"`
	}
	var raw repoConfigRaw
	if err := value.Decode(&raw); err != nil {
		return err
	}
	c.Agent = firstAgent(raw.Agent)
	c.Agents = copyAgents(raw.Agent)
	c.Commands = raw.Commands
	c.IgnorePatterns = raw.IgnorePatterns
	c.AllowRepoCommands = raw.AllowRepoCommands
	c.AutoFix = raw.AutoFix
	c.Intent = raw.Intent
	c.Test = raw.Test
	c.Document = raw.Document
	c.DisableProjectSettings = raw.DisableProjectSettings
	return nil
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
	Agents               []types.AgentName
	ACPXPath             string
	ACPRegistryOverrides map[string]string
	AgentPathOverride    map[string]string
	AgentArgsOverride    map[string][]string
	CITimeout            time.Duration
	StepQuietWarning     time.Duration
	LogLevel             string
	SessionReuse         bool
	Commands             Commands
	IgnorePatterns       []string
	AutoFix              AutoFix
	Intent               Intent
	Test                 Test
	Document             Document
	// DisableProjectSettings is the resolved, trusted-only opt-out (see the
	// RepoConfig field). When true, gate agents are launched with their
	// project-level settings/instructions suppressed; the daemon fails the run
	// closed if the resolved harness has no verified suppression knob.
	DisableProjectSettings bool
}

// Document is the resolved document-step config. Instructions come from the
// trusted default-branch repo config and augment the built-in placement
// policy in the document prompt.
type Document struct {
	Instructions string
}

// TestRaw is the YAML representation of test-step settings.
type TestRaw struct {
	Evidence EvidenceRaw `yaml:"evidence"`
}

// EvidenceRaw is the YAML representation of test-evidence settings.
// Pointer fields distinguish "not set" (nil) from explicit zero/false values.
type EvidenceRaw struct {
	StoreInRepo  *bool   `yaml:"store_in_repo"`
	Dir          *string `yaml:"dir"`
	UploadToGist *bool   `yaml:"upload_to_gist"`
}

// Test is the resolved test-step config.
type Test struct {
	Evidence Evidence
}

// Evidence is the resolved test-evidence config. StoreInRepo defaults true so
// reviewer-visible artifacts such as screenshots land in Dir (relative to the
// repo worktree), then are committed, pushed, and viewable directly on the PR.
// If StoreInRepo is explicitly disabled, evidence stays in a temporary
// directory; UploadToGist can publish local visual artifacts for GitHub PRs.
type Evidence struct {
	StoreInRepo  bool
	Dir          string
	UploadToGist bool
}

// IntentRaw is the YAML representation of user-intent extraction settings.
// Pointer fields distinguish "not set" (nil) from explicit zero/false values.
type IntentRaw struct {
	Enabled         *bool    `yaml:"enabled"`
	Threshold       *float64 `yaml:"threshold"`
	SlackDays       *int     `yaml:"slack_days"`
	DisabledReaders []string `yaml:"disabled_readers"`
}

// Intent is the resolved user-intent extraction config.
type Intent struct {
	Enabled         bool
	Threshold       float64
	SlackDays       int
	DisabledReaders map[string]bool
}

type agentList []types.AgentName

func (a *agentList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		name := strings.TrimSpace(value.Value)
		if name == "" {
			*a = nil
			return nil
		}
		*a = []types.AgentName{types.AgentName(name)}
		return nil
	case yaml.SequenceNode:
		names := make([]types.AgentName, 0, len(value.Content))
		for i, item := range value.Content {
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("agent[%d] must be a string", i)
			}
			name := strings.TrimSpace(item.Value)
			if name == "" {
				return fmt.Errorf("agent[%d] must not be empty", i)
			}
			names = append(names, types.AgentName(name))
		}
		*a = names
		return nil
	default:
		return fmt.Errorf("agent must be a string or a list of strings")
	}
}

func firstAgent(names []types.AgentName) types.AgentName {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func copyAgents(names []types.AgentName) []types.AgentName {
	if len(names) == 0 {
		return nil
	}
	out := make([]types.AgentName, len(names))
	copy(out, names)
	return out
}

// defaultConfigYAML is the template written when no global config file exists.
const defaultConfigYAML = `# no-mistakes global configuration

# Agent to use for code generation. This may also be an ordered fallback list,
# for example: agent: [codex, claude]
# Options: auto, claude, codex, rovodev, opencode, pi, copilot, acp:<target>
# "auto" detects the first available native agent on your system
# Use acp:<target> to run an optional user-installed acpx target, for example acp:gemini
agent: auto

# Optional path to the user-installed acpx binary for acp:<target> agents
# acpx_path: acpx

# Optional ACP target command overrides for acp:<target> agents
# acp_registry_overrides:
#   local-gemini: node /opt/mock-acp-agent.mjs

# Maximum time the CI monitor babysits an open PR with no base-branch movement
# before giving up. The monitor watches CI and auto-rebases when the base branch
# advances; each base advance re-arms this timer, so an actively-updated green PR
# keeps its monitor. Set to "unlimited", "none", "off", "never", or any
# non-positive duration to monitor until the PR is merged, closed, or the run is
# aborted with: no-mistakes axi abort --run <id>
ci_timeout: "168h"

# AXI status marks a running/fixing step as quiet when no step log or native
# agent lifecycle activity has appeared for this long. This is observability
# only; it never cancels work.
step_quiet_warning: "10m"

# Maximum time a CLI client waits for an existing daemon socket to accept a
# connection before failing instead of hanging.
daemon_connect_timeout: "3s"

# Reuse one durable agent session per run for the review loop: the reviewer
# keeps a single session across the initial review and every full rereview,
# and review fixes keep a separate fixer session. Roles never share a session.
# Supported for claude and codex; other agents run cold. Set false to force
# every agent invocation cold.
session_reuse: true

# Log level for daemon output
# Options: debug, info, warn, error
log_level: info

# Override native agent binary paths (optional)
# agent_path_override:
#   claude: /usr/local/bin/claude
#   codex: /opt/codex

# Extra native agent CLI flags (optional, global only)
# Codex service_tier controls speed/priority; model_reasoning_effort controls reasoning depth.
# agent_args_override:
#   codex:
#     - -m
#     - gpt-5.4
#     - -c
#     - service_tier="priority"
#     - -c
#     - model_reasoning_effort="low"
#
# Maximum follow-up auto-fix attempts per step (0 = disabled after the initial pass)
# Document fixes are attempted during the initial document pass.
auto_fix:
  rebase: 3
  lint: 3
  test: 3
  review: 0
  document: 3
  ci: 3

# User-intent extraction. When you push a branch, no-mistakes can read recent
# transcripts from your local agent (Claude Code, Codex, OpenCode, Rovo Dev, Pi,
# Copilot CLI), pick the session that produced the change, summarize the user
# intent, and feed it to review, test, document, lint, and PR agents so they
# understand what you were trying to do - not just the diff.
intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  # disabled_readers: [codex]

# Test-step evidence artifacts (screenshots, recordings, logs the test step
# gathers to demonstrate the change works). By default they are committed into
# the repo under a readable, branch-named directory so they are pushed and
# render directly on the PR. Secret-gist upload can still publish local visual
# artifacts for GitHub PRs when temp fallback evidence is approved.
test:
  evidence:
    upload_to_gist: true
    store_in_repo: true
    dir: .no-mistakes/evidence
`

// defaultBinary maps agent names to their default binary names.
var defaultBinary = map[types.AgentName]string{
	types.AgentClaude:   "claude",
	types.AgentCodex:    "codex",
	types.AgentRovoDev:  "acli",
	types.AgentOpenCode: "opencode",
	types.AgentPi:       "pi",
	types.AgentCopilot:  "copilot",
}

// agentProbeOrder is the priority order for auto-detecting agents.
var agentProbeOrder = []types.AgentName{
	types.AgentClaude,
	types.AgentCodex,
	types.AgentOpenCode,
	types.AgentRovoDev,
	types.AgentPi,
	types.AgentCopilot,
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
	winproc.Harden(cmd)
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

// ResolveAgent resolves configured agent names to available agents. A single
// explicit agent must be runnable; auto is probed into the first available
// native agent; an ordered list is filtered to available agents and kept as fallbacks.
// The lookPath function should behave like exec.LookPath.
func (c *Config) ResolveAgent(ctx context.Context, lookPath func(string) (string, error)) error {
	candidates := c.configuredAgents()
	if len(candidates) <= 1 {
		c.Agent = firstAgent(candidates)
		c.Agents = copyAgents(candidates)
		if c.Agent == types.AgentAuto {
			name, err := c.resolveAutoAgent(ctx, lookPath)
			if err != nil {
				return err
			}
			c.Agent = name
			c.Agents = []types.AgentName{name}
			return nil
		}
		name, ok, probe, err := c.resolveConfiguredAgent(ctx, c.Agent, lookPath)
		if err != nil {
			return err
		}
		if !ok {
			return noRunnableAgentError([]types.AgentName{c.Agent}, []string{probe})
		}
		c.Agent = name
		c.Agents = []types.AgentName{name}
		return nil
	}

	resolved, err := c.resolveAgentList(ctx, candidates, lookPath)
	if err != nil {
		return err
	}
	c.Agent = resolved[0]
	c.Agents = resolved
	return nil
}

func (c *Config) configuredAgents() []types.AgentName {
	if len(c.Agents) > 0 {
		return copyAgents(c.Agents)
	}
	if c.Agent != "" {
		return []types.AgentName{c.Agent}
	}
	return []types.AgentName{types.AgentAuto}
}

func (c *Config) resolveAutoAgent(ctx context.Context, lookPath func(string) (string, error)) (types.AgentName, error) {
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
					return "", probeErr
				}
				if !ok {
					continue
				}
			}
			return name, nil
		} else if !errors.Is(err, exec.ErrNotFound) && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("resolve %s agent from %q: %w", name, bin, err)
		}
	}
	return "", noRunnableAgentError([]types.AgentName{types.AgentAuto}, probed)
}

func (c *Config) resolveAgentList(ctx context.Context, candidates []types.AgentName, lookPath func(string) (string, error)) ([]types.AgentName, error) {
	resolved := make([]types.AgentName, 0, len(candidates))
	seen := map[types.AgentName]bool{}
	probed := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		name, ok, probe, err := c.resolveConfiguredAgent(ctx, candidate, lookPath)
		if probe != "" {
			probed = append(probed, probe)
		}
		if err != nil {
			return nil, err
		}
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		resolved = append(resolved, name)
	}
	if len(resolved) == 0 {
		return nil, noRunnableAgentError(candidates, probed)
	}
	return resolved, nil
}

func noRunnableAgentError(configured []types.AgentName, probed []string) error {
	names := make([]string, 0, len(configured))
	for _, name := range configured {
		names = append(names, string(name))
	}
	return fmt.Errorf(
		"no runnable agent found for configured agent %s (looked for: %s); the gate cannot validate without an agent; install a supported native agent, choose an available agent in ~/.no-mistakes/config.yaml, or configure agent: acp:<target> with acpx installed",
		strings.Join(names, ", "),
		strings.Join(probed, ", "),
	)
}

func (c *Config) resolveConfiguredAgent(ctx context.Context, name types.AgentName, lookPath func(string) (string, error)) (types.AgentName, bool, string, error) {
	if name == types.AgentAuto {
		resolved, err := c.resolveAutoAgent(ctx, lookPath)
		if err != nil && strings.HasPrefix(err.Error(), "no runnable agent found") {
			return "", false, "auto", nil
		}
		return resolved, err == nil, "auto", err
	}
	if _, ok := defaultBinary[name]; !ok && !isACPAgent(name) {
		return "", false, string(name), fmt.Errorf("unknown agent %q; valid options: auto, claude, codex, rovodev, opencode, pi, copilot, acp:<target> (set 'agent' in ~/.no-mistakes/config.yaml)", name)
	}
	bin := c.AgentPathFor(name)
	resolvedBin, err := lookPath(bin)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return "", false, bin, nil
		}
		return "", false, bin, fmt.Errorf("resolve %s agent from %q: %w", name, bin, err)
	}
	if name == types.AgentRovoDev {
		ok, probeErr := probeRovoDevSupport(ctx, resolvedBin)
		if probeErr != nil {
			return "", false, bin, probeErr
		}
		if !ok {
			return "", false, bin, nil
		}
	}
	return name, true, bin, nil
}

// AgentPath returns the binary path for the configured agent.
// ACP agents use acpx_path if set, otherwise acpx.
// Native agents use agent_path_override if set, otherwise the default binary name.
func (c *Config) AgentPath() string {
	return c.AgentPathFor(c.Agent)
}

func (c *Config) AgentPathFor(name types.AgentName) string {
	if isACPAgent(name) {
		if c.ACPXPath != "" {
			return c.ACPXPath
		}
		return "acpx"
	}
	if c.AgentPathOverride != nil {
		if p, ok := c.AgentPathOverride[string(name)]; ok {
			return p
		}
	}
	if b, ok := defaultBinary[name]; ok {
		return b
	}
	return string(name)
}

// AgentArgs returns extra CLI args for the configured native agent, as declared in
// agent_args_override. Returns nil when no override is set for this agent.
func (c *Config) AgentArgs() []string {
	return c.AgentArgsFor(c.Agent)
}

func (c *Config) AgentArgsFor(name types.AgentName) []string {
	if c.AgentArgsOverride == nil {
		return nil
	}
	return c.AgentArgsOverride[string(name)]
}

// agentArgsOverrideAgents lists native agent names accepted as keys in
// agent_args_override.
var agentArgsOverrideAgents = map[string]bool{
	string(types.AgentClaude):   true,
	string(types.AgentCodex):    true,
	string(types.AgentRovoDev):  true,
	string(types.AgentOpenCode): true,
	string(types.AgentPi):       true,
	string(types.AgentCopilot):  true,
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
		"-r":              true,
		"--resume":        true,
		"--session-id":    true,
		"-c":              true,
		"--continue":      true,
		"--fork-session":  true,
	},
	string(types.AgentCodex): {
		"exec":         true,
		"resume":       true,
		"--resume":     true,
		"--session":    true,
		"--session-id": true,
		"--thread":     true,
		"--thread-id":  true,
		"--last":       true,
		"--json":       true,
		"--color":      true,
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
	string(types.AgentCopilot): {
		"-p":              true,
		"--prompt":        true,
		"--output-format": true,
		"--no-color":      true,
	},
}

// validateAgentArgsOverride ensures each agent key is a known agent name and
// that no reserved flag appears. Empty args are rejected to catch trivially
// broken YAML.
func validateAgentArgsOverride(override map[string][]string) error {
	for name, args := range override {
		if !agentArgsOverrideAgents[name] {
			return fmt.Errorf("invalid agent name in agent_args_override: %q (valid: claude, codex, rovodev, opencode, pi, copilot)", name)
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

// DefaultGlobalConfig returns the built-in global defaults.
func DefaultGlobalConfig() *GlobalConfig {
	return &GlobalConfig{
		Agent:                types.AgentAuto,
		Agents:               []types.AgentName{types.AgentAuto},
		CITimeout:            DefaultCITimeout,
		StepQuietWarning:     DefaultStepQuietWarning,
		DaemonConnectTimeout: DefaultDaemonConnectTimeout,
		LogLevel:             "info",
		SessionReuse:         true,
	}
}

// LoadGlobal reads global config from path. Returns defaults if file doesn't exist.
func LoadGlobal(path string) (*GlobalConfig, error) {
	cfg := DefaultGlobalConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read global config: %w", err)
	}

	var raw globalConfigRaw
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse global config: %w", err)
	}

	if len(raw.Agent) > 0 {
		cfg.Agents = copyAgents(raw.Agent)
		cfg.Agent = firstAgent(cfg.Agents)
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
		d, err := parseCITimeout(timeoutValue)
		if err != nil {
			return nil, err
		}
		cfg.CITimeout = d
	}
	if raw.StepQuietWarning != "" {
		d, err := time.ParseDuration(raw.StepQuietWarning)
		if err != nil {
			return nil, fmt.Errorf("parse step_quiet_warning %q: %w", raw.StepQuietWarning, err)
		}
		if d > 0 {
			cfg.StepQuietWarning = d
		}
	}
	if raw.DaemonConnectTimeout != "" {
		d, err := parsePositiveDuration("daemon_connect_timeout", raw.DaemonConnectTimeout)
		if err != nil {
			return nil, err
		}
		cfg.DaemonConnectTimeout = d
	}
	if raw.LogLevel != "" {
		cfg.LogLevel = raw.LogLevel
	}
	if raw.SessionReuse != nil {
		cfg.SessionReuse = *raw.SessionReuse
	}
	if raw.AutoFix.CI == nil {
		raw.AutoFix.CI = raw.AutoFix.Babysit
	}
	cfg.AutoFix = raw.AutoFix
	cfg.Intent = raw.Intent
	cfg.Test = raw.Test

	return cfg, nil
}

// parseCITimeout interprets the ci_timeout config value. The keyword
// "unlimited" (also "none"/"off"/"never"), or any non-positive duration,
// resolves to CITimeoutUnlimited so the monitor never self-terminates;
// otherwise the value is parsed as a Go duration.
func parseCITimeout(value string) (time.Duration, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "unlimited", "none", "off", "never":
		return CITimeoutUnlimited, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse ci_timeout %q: %w", value, err)
	}
	if d <= 0 {
		return CITimeoutUnlimited, nil
	}
	return d, nil
}

func parsePositiveDuration(name, value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s %q: %w", name, value, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("parse %s %q: duration must be positive", name, value)
	}
	return d, nil
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

	return parseRepoConfig(data)
}

// LoadRepoFromBytes parses per-repo config from raw YAML bytes. It is the
// trusted-config entry point: callers that read .no-mistakes.yaml from a
// specific git ref (e.g. the default branch) use this to avoid honoring a
// contributor's checked-out copy.
func LoadRepoFromBytes(data []byte) (*RepoConfig, error) {
	return parseRepoConfig(data)
}

func parseRepoConfig(data []byte) (*RepoConfig, error) {
	cfg := &RepoConfig{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse repo config: %w", err)
	}
	if cfg.AutoFix.CI == nil {
		cfg.AutoFix.CI = cfg.AutoFix.Babysit
	}

	return cfg, nil
}

// EffectiveRepoConfig returns the repo config that should drive the pipeline
// given a pushed-branch copy and the trusted default-branch copy.
//
// The code-executing selection fields - Commands (run verbatim via sh -c on
// the daemon host) and Agent/Agents (select which processes launch with the
// maintainer's credentials, including fallback lists and acp: targets) - are
// taken only from the trusted copy when it is present, so a contributor's
// pushed branch cannot inject shell or pick an agent. Document (the
// documentation placement policy injected into the document gate prompt) is
// trusted-only for the same reason: a pushed branch must not weaken the
// documentation rules that gate itself. DisableProjectSettings is also
// trusted-only so a pushed branch cannot enable or defeat the gate-agent
// project-instruction boundary. When allowRepoCommands is
// true the maintainer has explicitly opted in (via allow_repo_commands on the
// TRUSTED default-branch copy) to honoring the pushed branch's commands and
// agent selection.
// When there is no trusted copy and the maintainer has not opted in, both
// fields are forced empty (Agent "" and nil Agents inherit the global agent;
// Commands{} yields built-in defaults) rather than falling back to the pushed
// branch - this blocks the supply-chain vector for repos that ship
// .no-mistakes.yaml only on feature branches.
//
// Non-executing fields (ignore patterns, auto-fix, intent, test) are always
// taken from the pushed copy, matching prior behavior, since they cannot
// run arbitrary shell or select a process.
func EffectiveRepoConfig(pushed, trusted *RepoConfig, allowRepoCommands bool) *RepoConfig {
	if pushed == nil {
		pushed = &RepoConfig{}
	}
	effective := *pushed
	if trusted != nil {
		effective.Document = trusted.Document
		// disable_project_settings is a security boundary: honor it ONLY from the
		// trusted default-branch copy so a pushed branch cannot turn the opt-out
		// off (and re-enable its own AGENTS.md) or on. A nil trusted copy here
		// means the trusted config was legitimately absent (the daemon aborts
		// separately when it could not be READ at all), so falsy is correct.
		effective.DisableProjectSettings = trusted.DisableProjectSettings
	} else {
		effective.Document = DocumentRaw{}
		effective.DisableProjectSettings = false
	}
	if allowRepoCommands {
		return &effective
	}
	if trusted != nil {
		effective.Commands = trusted.Commands
		effective.Agent = trusted.Agent
		effective.Agents = copyAgents(trusted.Agents)
	} else {
		effective.Commands = Commands{}
		effective.Agent = ""
		effective.Agents = nil
	}
	return &effective
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

// intentDefaults returns the default user-intent extraction settings.
// Default-on with a moderate file-overlap threshold and a 3-day slack window
// to handle "agent generated change Monday, user pushed Wednesday" cases.
func intentDefaults() Intent {
	return Intent{
		Enabled:         true,
		Threshold:       0.2,
		SlackDays:       3,
		DisabledReaders: map[string]bool{},
	}
}

// applyIntentOverrides applies non-nil raw values onto resolved defaults.
func applyIntentOverrides(dst *Intent, src *IntentRaw) {
	if src.Enabled != nil {
		dst.Enabled = *src.Enabled
	}
	if src.Threshold != nil {
		dst.Threshold = *src.Threshold
	}
	if src.SlackDays != nil {
		dst.SlackDays = *src.SlackDays
	}
	if len(src.DisabledReaders) > 0 {
		if dst.DisabledReaders == nil {
			dst.DisabledReaders = map[string]bool{}
		}
		for _, name := range src.DisabledReaders {
			dst.DisabledReaders[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
}

// testDefaults returns the default test-step settings. Evidence storage is
// in-repo by default so screenshots and other reviewer-visible artifacts render
// on PRs instead of leaking local temp paths.
func testDefaults() Test {
	return Test{
		Evidence: Evidence{
			StoreInRepo:  true,
			Dir:          ".no-mistakes/evidence",
			UploadToGist: true,
		},
	}
}

// applyTestOverrides applies non-nil raw values onto resolved defaults.
func applyTestOverrides(dst *Test, src *TestRaw) {
	if src.Evidence.StoreInRepo != nil {
		dst.Evidence.StoreInRepo = *src.Evidence.StoreInRepo
	}
	if src.Evidence.Dir != nil && strings.TrimSpace(*src.Evidence.Dir) != "" {
		dst.Evidence.Dir = strings.TrimSpace(*src.Evidence.Dir)
	}
	if src.Evidence.UploadToGist != nil {
		dst.Evidence.UploadToGist = *src.Evidence.UploadToGist
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

// Merge combines global and per-repo config. Per-repo agent values, including
// ordered fallback lists, override global agent values when non-empty. Commands
// and ignore patterns come from repo config only.
func Merge(global *GlobalConfig, repo *RepoConfig) *Config {
	af := autoFixDefaults()
	applyAutoFixOverrides(&af, &global.AutoFix)
	applyAutoFixOverrides(&af, &repo.AutoFix)

	intent := intentDefaults()
	applyIntentOverrides(&intent, &global.Intent)
	applyIntentOverrides(&intent, &repo.Intent)

	test := testDefaults()
	applyTestOverrides(&test, &global.Test)
	applyTestOverrides(&test, &repo.Test)

	cfg := &Config{
		Agent:                global.Agent,
		Agents:               copyAgents(global.Agents),
		ACPXPath:             global.ACPXPath,
		ACPRegistryOverrides: global.ACPRegistryOverrides,
		AgentPathOverride:    global.AgentPathOverride,
		AgentArgsOverride:    global.AgentArgsOverride,
		CITimeout:            global.CITimeout,
		StepQuietWarning:     global.StepQuietWarning,
		LogLevel:             global.LogLevel,
		SessionReuse:         global.SessionReuse,
		Commands:             repo.Commands,
		IgnorePatterns:       repo.IgnorePatterns,
		AutoFix:              af,
		Intent:               intent,
		Test:                 test,
		Document:             Document{Instructions: strings.TrimSpace(repo.Document.Instructions)},
		// repo is the EffectiveRepoConfig result, so this value is already
		// trusted-only (EffectiveRepoConfig sourced it from the trusted copy).
		DisableProjectSettings: repo.DisableProjectSettings,
	}

	if repo.Agent != "" {
		cfg.Agent = repo.Agent
		cfg.Agents = copyAgents(repo.Agents)
		if len(cfg.Agents) == 0 {
			cfg.Agents = []types.AgentName{repo.Agent}
		}
	}

	return cfg
}
