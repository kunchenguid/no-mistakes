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
	// CITimeoutUnlimited is the sentinel meaning "monitor until the PR is
	// merged, closed, or the run is aborted - never self-terminate".
	// Any non-positive ci_timeout, or the keywords "unlimited", "none",
	// "off", and "never", resolves to this.
	CITimeoutUnlimited = time.Duration(-1)
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
	Intent               IntentRaw
	Test                 TestRaw
	Review               ReviewRaw
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
	Intent               IntentRaw           `yaml:"intent"`
	Test                 TestRaw             `yaml:"test"`
	Review               ReviewRaw           `yaml:"review"`
}

// RepoConfig represents .no-mistakes.yaml in a repo root.
type RepoConfig struct {
	Agent          types.AgentName `yaml:"agent"`
	Commands       Commands        `yaml:"commands"`
	IgnorePatterns []string        `yaml:"ignore_patterns"`
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
	// Review is a pointer so an absent review block (nil) is distinguishable
	// from an explicit empty one (&ReviewRaw{}). An explicit repo-level
	// review block - including review.reviewers: [] - overrides the inherited
	// global panel; an empty reviewer list disables the panel and reverts to
	// the single-agent default. When the key is absent the repo inherits the
	// global review config (see Merge).
	Review *ReviewRaw `yaml:"review"`
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
	Intent               Intent
	Test                 Test
	Review               Review
}

// ReviewerSpec identifies one reviewer in the cross-family review panel. Agent
// selects the reviewer family (a native agent name or acp:<target>). Args and
// Path optionally override the per-agent CLI flags and binary path for this
// reviewer, taking precedence over agent_args_override / agent_path_override
// keyed by the agent name (so two same-family reviewers can run on different
// models).
type ReviewerSpec struct {
	Agent types.AgentName `yaml:"agent"`
	Args  []string        `yaml:"args"`
	Path  string          `yaml:"path"`
}

// ReviewRaw is the YAML representation of the multi-reviewer panel. An empty
// Reviewers list means the single-agent default (review runs once on the
// configured agent). On RepoConfig the block's presence is tracked via a
// pointer, so an explicit empty list disables an inherited global panel while an
// absent block inherits it (see RepoConfig.Review and Merge).
type ReviewRaw struct {
	Reviewers   []ReviewerSpec `yaml:"reviewers"`
	MaxParallel int            `yaml:"max_parallel"`
	FailOpen    *bool          `yaml:"fail_open"`
}

// Review is the resolved multi-reviewer panel config. FailOpen defaults to
// false (fail-closed): a reviewer error fails the review step rather than
// silently dropping a reviewer.
type Review struct {
	Reviewers   []ReviewerSpec
	MaxParallel int
	FailOpen    bool
}

// TestRaw is the YAML representation of test-step settings.
type TestRaw struct {
	Evidence EvidenceRaw `yaml:"evidence"`
}

// EvidenceRaw is the YAML representation of test-evidence settings.
// Pointer fields distinguish "not set" (nil) from explicit zero/false values.
type EvidenceRaw struct {
	StoreInRepo *bool   `yaml:"store_in_repo"`
	Dir         *string `yaml:"dir"`
}

// Test is the resolved test-step config.
type Test struct {
	Evidence Evidence
}

// Evidence is the resolved test-evidence config. When StoreInRepo is true, the
// test step writes evidence artifacts into Dir (relative to the repo worktree)
// so they are committed, pushed, and viewable directly on the PR. Otherwise
// evidence stays in a temporary directory referenced only by local path.
type Evidence struct {
	StoreInRepo bool
	Dir         string
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

// defaultConfigYAML is the template written when no global config file exists.
const defaultConfigYAML = `# no-mistakes global configuration

# Agent to use for code generation
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

# Log level for daemon output
# Options: debug, info, warn, error
log_level: info

# Override native agent binary paths (optional)
# agent_path_override:
#   claude: /usr/local/bin/claude
#   codex: /opt/codex

# Extra native agent CLI flags (optional, global only)
# agent_args_override:
#   codex:
#     - -m
#     - gpt-5.4
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

# Cross-family review panel (optional). When set, each reviewer independently
# reviews the same diff; all reports go to the single fix agent + the human to
# reconcile. With no reviewers configured (the default), review runs once on the
# 'agent' above, so behavior is unchanged. Per-reviewer path/args fall back to
# agent_path_override / agent_args_override keyed by the reviewer's agent name;
# set path/args per reviewer to run two same-family reviewers on different
# models.
# review:
#   reviewers:
#     - agent: codex
#     - agent: claude
#   max_parallel: 2   # bound concurrent reviewers; 0 = all at once
#   fail_open: false  # any reviewer error fails the step (safe default)

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
# gathers to demonstrate the change works). By default they are kept in a
# temporary directory and referenced by local path. Opt in to store_in_repo to
# commit them into the repo under a readable, branch-named directory so they are
# pushed and render directly on the PR.
# test:
#   evidence:
#     store_in_repo: true
#     dir: .no-mistakes/evidence
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

// AgentArgs returns extra CLI args for the configured native agent, as declared in
// agent_args_override. Returns nil when no override is set for this agent.
func (c *Config) AgentArgs() []string {
	if c.AgentArgsOverride == nil {
		return nil
	}
	return c.AgentArgsOverride[string(c.Agent)]
}

// ResolveReviewers resolves the configured review panel into concrete reviewer
// specs. It mirrors ResolveAgent: each spec's agent must be a concrete native
// family or acp:<target>. A bare "auto" reviewer cannot itself probe the
// system, so it is expanded to the already-resolved single agent (c.Agent) when
// one exists and rejected otherwise. rovodev reviewers are validated with
// probeRovoDevSupport (reusing the same lookPath the single-agent resolution
// uses). Identical specs (same agent, path, args) are de-duplicated so a panel
// never runs the same reviewer twice. The lookPath function should behave like
// exec.LookPath. Returns nil when no reviewers are configured.
func (c *Config) ResolveReviewers(ctx context.Context, lookPath func(string) (string, error)) ([]ReviewerSpec, error) {
	if len(c.Review.Reviewers) == 0 {
		return nil, nil
	}
	resolved := make([]ReviewerSpec, 0, len(c.Review.Reviewers))
	seen := make(map[string]bool, len(c.Review.Reviewers))
	for i, spec := range c.Review.Reviewers {
		if spec.Agent == types.AgentAuto {
			if c.Agent == "" || c.Agent == types.AgentAuto {
				return nil, fmt.Errorf("review.reviewers[%d]: agent %q cannot be auto-resolved; set 'agent' to a concrete value or name the reviewer family explicitly", i, types.AgentAuto)
			}
			spec.Agent = c.Agent
		}
		// Validate against the concrete resolved family before the spec reaches
		// the real reviewer command. ResolveReviewers is the authoritative
		// post-trust, post-expansion anchor: the untrusted pushed-config path
		// skips validateReviewers, so a concrete-family reviewer under
		// allow_repo_commands reaches here with no prior empty-arg, unknown-agent,
		// or reserved-arg check. validateReviewerSpec is the single shared check
		// so this path and validateReviewers cannot drift.
		if err := validateReviewerSpec(i, spec); err != nil {
			return nil, err
		}
		if spec.Agent == types.AgentRovoDev {
			bin := c.ReviewerPath(spec)
			resolvedBin, err := lookPath(bin)
			if err != nil {
				return nil, fmt.Errorf("review.reviewers[%d]: resolve rovodev from %q: %w", i, bin, err)
			}
			ok, probeErr := probeRovoDevSupport(ctx, resolvedBin)
			if probeErr != nil {
				return nil, probeErr
			}
			if !ok {
				return nil, fmt.Errorf("review.reviewers[%d]: %q does not support the rovodev subcommand", i, resolvedBin)
			}
		}
		// Dedup on the EFFECTIVE reviewer (after auto expansion and the
		// ReviewerPath / ReviewerArgs fallbacks), so a spec that inherits its
		// path/args from agent_path_override / agent_args_override collides with
		// an explicit spec that resolves to the same binary and args.
		key := reviewerDedupKey(ReviewerSpec{
			Agent: spec.Agent,
			Path:  c.ReviewerPath(spec),
			Args:  c.ReviewerArgs(spec),
		})
		if seen[key] {
			continue
		}
		seen[key] = true
		resolved = append(resolved, spec)
	}
	return resolved, nil
}

// ReviewerPath returns the binary path for a reviewer spec. A per-spec Path
// wins; otherwise it falls back to agent_path_override keyed by the agent name,
// then the default binary name (or acpx for acp: targets) - mirroring
// AgentPath.
func (c *Config) ReviewerPath(spec ReviewerSpec) string {
	if spec.Path != "" {
		return spec.Path
	}
	if isACPAgent(spec.Agent) {
		if c.ACPXPath != "" {
			return c.ACPXPath
		}
		return "acpx"
	}
	if c.AgentPathOverride != nil {
		if p, ok := c.AgentPathOverride[string(spec.Agent)]; ok {
			return p
		}
	}
	if b, ok := defaultBinary[spec.Agent]; ok {
		return b
	}
	return string(spec.Agent)
}

// ReviewerArgs returns the extra native CLI args for a reviewer spec. Per-spec
// Args win; otherwise they fall back to agent_args_override keyed by the agent
// name - mirroring AgentArgs.
func (c *Config) ReviewerArgs(spec ReviewerSpec) []string {
	if len(spec.Args) > 0 {
		return spec.Args
	}
	if c.AgentArgsOverride == nil {
		return nil
	}
	return c.AgentArgsOverride[string(spec.Agent)]
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

// isNativeAgent reports whether name is a known native agent family (one with a
// default binary), as opposed to "auto" or an acp:<target>.
func isNativeAgent(name types.AgentName) bool {
	_, ok := defaultBinary[name]
	return ok
}

// validateReviewers checks the configured review panel. Each reviewer must name
// a known native agent family or an acp:<target> ("auto" is permitted here and
// resolved later by ResolveReviewers). Per-spec Args may not contain a flag
// reserved by no-mistakes - the same reservation applied to agent_args_override
// - nor an empty arg.
func validateReviewers(reviewers []ReviewerSpec) error {
	for i, spec := range reviewers {
		if err := validateReviewerSpec(i, spec); err != nil {
			return err
		}
	}
	return nil
}

// validateReviewerSpec is the single shared check for one reviewer spec, called
// from both validateReviewers (the trusted load-time check, which permits a bare
// "auto") and ResolveReviewers (the authoritative post-trust, post-expansion
// check). Keeping one helper means the two documented validation points cannot
// drift. It rejects an empty agent, an unknown family, an empty/whitespace arg,
// and any arg reserved by no-mistakes for the family. For a concrete family the
// reserved set is known now; for a bare "auto" reviewer the family is only known
// after ResolveReviewers expands it, so reservedArgViolation re-runs there
// against the resolved family - never trust an "auto" arg validated against the
// empty reserved set.
func validateReviewerSpec(i int, spec ReviewerSpec) error {
	name := string(spec.Agent)
	if name == "" {
		return fmt.Errorf("invalid review.reviewers[%d]: missing agent", i)
	}
	if spec.Agent != types.AgentAuto && !isACPAgent(spec.Agent) && !isNativeAgent(spec.Agent) {
		return fmt.Errorf("invalid review.reviewers[%d]: unknown agent %q (valid: auto, claude, codex, rovodev, opencode, pi, acp:<target>)", i, name)
	}
	for j, arg := range spec.Args {
		if strings.TrimSpace(arg) == "" {
			return fmt.Errorf("invalid review.reviewers[%d].args[%d]: empty arg", i, j)
		}
	}
	if arg, j, bad := reservedArgViolation(name, spec.Args); bad {
		return fmt.Errorf("invalid review.reviewers[%d].args[%d]: %q is managed by no-mistakes and cannot be overridden", i, j, arg)
	}
	return nil
}

// reservedArgViolation reports whether any arg in args is a flag reserved by
// no-mistakes for the named agent family. A flag matches by its bare form
// (e.g. "--json") as well as the "--json=value" form. The returned index is the
// position in args. When name is "auto" (or any family with no reserved set)
// nothing matches, which is why ResolveReviewers must re-run this check after
// expanding "auto" to a concrete family.
func reservedArgViolation(name string, args []string) (string, int, bool) {
	reserved := reservedAgentArgs[name]
	for j, arg := range args {
		base := arg
		if idx := strings.Index(arg, "="); idx > 0 {
			base = arg[:idx]
		}
		if reserved[base] {
			return arg, j, true
		}
	}
	return "", 0, false
}

// reviewerDedupKey produces a stable identity for a reviewer spec so a panel
// never runs two identical reviewers. Specs are identical when their agent,
// path, and args all match.
func reviewerDedupKey(spec ReviewerSpec) string {
	return string(spec.Agent) + "\x00" + spec.Path + "\x00" + strings.Join(spec.Args, "\x01")
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
		CITimeout: DefaultCITimeout,
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
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
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
		d, err := parseCITimeout(timeoutValue)
		if err != nil {
			return nil, err
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
	cfg.Intent = raw.Intent
	cfg.Test = raw.Test
	if err := validateReviewers(raw.Review.Reviewers); err != nil {
		return nil, err
	}
	cfg.Review = raw.Review

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
// contributor's checked-out copy. Because the bytes are trusted, the review
// panel is semantically validated here; the untrusted pushed-branch path
// (LoadRepo) deliberately skips that check so a contributor cannot fail a run
// with a review block that EffectiveRepoConfig will strip anyway.
func LoadRepoFromBytes(data []byte) (*RepoConfig, error) {
	cfg, err := parseRepoConfig(data)
	if err != nil {
		return nil, err
	}
	if cfg.Review != nil {
		if err := validateReviewers(cfg.Review.Reviewers); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// parseRepoConfig unmarshals per-repo config without semantically validating the
// review panel. The review block is code-executing config taken only from the
// trusted default-branch copy (EffectiveRepoConfig), so validation belongs to
// the trusted entry point (LoadRepoFromBytes) and to ResolveReviewers, not to
// every parse of a possibly-untrusted pushed-branch file.
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
// The code-executing selection fields — Commands (run verbatim via sh -c on
// the daemon host), Agent (selects which process launches with the
// maintainer's credentials, including acp: targets), and Review (the review
// panel, which likewise selects which reviewer binaries launch) — are taken
// only from the trusted copy when it is present, so a contributor's pushed
// branch cannot inject shell or pick an agent. When allowRepoCommands is true the
// maintainer has explicitly opted in (via allow_repo_commands on the
// TRUSTED default-branch copy) to honoring the pushed-branch copy wholesale.
// When there is no trusted copy and the maintainer has not opted in, both
// fields are forced empty (Agent "" inherits the global agent; Commands{}
// yields built-in defaults; Review nil inherits the global panel) rather than
// falling back to the pushed branch — this blocks the supply-chain vector for
// repos that ship .no-mistakes.yaml only on feature branches.
//
// Non-executing fields (ignore patterns, auto-fix, intent, test) are always
// taken from the pushed copy, matching prior behavior, since they cannot
// run arbitrary shell or select a process.
func EffectiveRepoConfig(pushed, trusted *RepoConfig, allowRepoCommands bool) *RepoConfig {
	if pushed == nil {
		pushed = &RepoConfig{}
	}
	effective := *pushed
	if allowRepoCommands {
		return &effective
	}
	if trusted != nil {
		effective.Commands = trusted.Commands
		effective.Agent = trusted.Agent
		// SECURITY: the review panel selects which reviewer binaries launch
		// with the maintainer's credentials, so it is code-executing config.
		// Take it from the trusted default-branch copy, never the pushed SHA.
		effective.Review = trusted.Review
	} else {
		effective.Commands = Commands{}
		effective.Agent = ""
		effective.Review = nil
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
// opt-in (off by default); when enabled it lands under .no-mistakes/evidence.
func testDefaults() Test {
	return Test{
		Evidence: Evidence{
			StoreInRepo: false,
			Dir:         ".no-mistakes/evidence",
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
}

// resolveReview converts a raw review panel into its resolved form. FailOpen
// defaults to false (fail-closed) when unset.
func resolveReview(raw ReviewRaw) Review {
	failOpen := false
	if raw.FailOpen != nil {
		failOpen = *raw.FailOpen
	}
	return Review{
		Reviewers:   raw.Reviewers,
		MaxParallel: raw.MaxParallel,
		FailOpen:    failOpen,
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

	intent := intentDefaults()
	applyIntentOverrides(&intent, &global.Intent)
	applyIntentOverrides(&intent, &repo.Intent)

	test := testDefaults()
	applyTestOverrides(&test, &global.Test)
	applyTestOverrides(&test, &repo.Test)

	// Default the review panel from global; an explicit repo review block
	// overrides it wholesale, including an empty reviewer list (which disables
	// the inherited global panel and reverts to the single-agent default). An
	// absent repo review block (nil) inherits global. Both copies are trusted by
	// the time they reach Merge (EffectiveRepoConfig strips a pushed-branch
	// review block to the trusted default-branch copy).
	review := resolveReview(global.Review)
	if repo.Review != nil {
		review = resolveReview(*repo.Review)
	}

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
		Intent:               intent,
		Test:                 test,
		Review:               review,
	}

	if repo.Agent != "" {
		cfg.Agent = repo.Agent
	}

	return cfg
}
