package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
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

// GlobalConfig represents ~/.no-mistakes/config.yaml. Model selection is owned
// entirely by the routing contract (Runners, Profiles, Candidates, Routes);
// there is no single-agent selector, fallback list, or per-agent override.
type GlobalConfig struct {
	CITimeout            time.Duration `yaml:"-"`
	StepQuietWarning     time.Duration `yaml:"-"`
	DaemonConnectTimeout time.Duration `yaml:"-"`
	LogLevel             string        `yaml:"log_level"`
	// SessionReuse controls per-run, per-role agent session reuse in the
	// review loop (one durable reviewer session across full reviews, a
	// separate durable fixer session across fix turns). Default true; set
	// session_reuse: false to force every invocation cold.
	SessionReuse bool `yaml:"-"`
	Intent       IntentRaw
	Test         TestRaw
	Routing      RoutingConfig
}

// globalConfigRaw is the on-disk YAML representation with duration as string.
type globalConfigRaw struct {
	CITimeout            string         `yaml:"ci_timeout"`
	DaemonConnectTimeout string         `yaml:"daemon_connect_timeout"`
	BabysitTimeout       string         `yaml:"babysit_timeout"`
	StepQuietWarning     string         `yaml:"step_quiet_warning"`
	LogLevel             string         `yaml:"log_level"`
	SessionReuse         *bool          `yaml:"session_reuse"`
	Intent               IntentRaw      `yaml:"intent"`
	Test                 TestRaw        `yaml:"test"`
	Routing              *RoutingConfig `yaml:"routing"`
}

// RepoConfig represents .no-mistakes.yaml in a repo root. A repository may map
// purposes to existing global profiles via 'routes', but never selects an
// agent or defines execution mechanics.
type RepoConfig struct {
	Commands       Commands `yaml:"commands"`
	IgnorePatterns []string `yaml:"ignore_patterns"`
	// AllowRepoCommands opts in to honoring the code-executing selection
	// fields (commands.{test,lint,format}) from a contributor's pushed branch
	// instead of the trusted default-branch copy. It is read ONLY from the
	// trusted default-branch copy of .no-mistakes.yaml (never the pushed SHA),
	// so a contributor cannot self-enable. Default false: the pushed branch
	// controls nothing that executes.
	AllowRepoCommands bool                          `yaml:"allow_repo_commands"`
	Intent            IntentRaw                     `yaml:"intent"`
	Test              TestRaw                       `yaml:"test"`
	Routes            map[types.Purpose]ProfileName `yaml:"routes"`
	// Document carries the repository's documentation placement policy. It
	// steers the document step's gate prompt, so it is honored ONLY from the
	// trusted default-branch copy of .no-mistakes.yaml (see
	// EffectiveRepoConfig): a contributor's pushed branch must not be able to
	// weaken documentation rules for its own review.
	Document DocumentRaw `yaml:"document"`
}

// DocumentRaw is the YAML representation of document-step settings.
type DocumentRaw struct {
	// Instructions augment (never replace) the built-in documentation
	// placement policy with the repository's ownership map or extra
	// placement rules.
	Instructions string `yaml:"instructions"`
}



// Commands holds optional per-repo command overrides.
type Commands struct {
	Lint   string `yaml:"lint"`
	Test   string `yaml:"test"`
	Format string `yaml:"format"`
}

// Config is the merged result of global + per-repo configuration.
type Config struct {
	CITimeout            time.Duration
	StepQuietWarning     time.Duration
	DaemonConnectTimeout time.Duration
	LogLevel             string
	SessionReuse         bool
	Commands             Commands
	IgnorePatterns       []string
	Intent               Intent
	Test                 Test
	Document             Document
	Routing              RoutingConfig
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
// Model selection is intentionally absent: the built-in routing contract
// applies unless overridden under 'routing'.
const defaultConfigYAML = `# no-mistakes global configuration


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


# User-intent extraction. When you push a branch, no-mistakes can read recent
# transcripts from your local agent, pick the session that produced the change,
# summarize the user intent, and feed it to the routed review, test, document,
# lint, and PR invocations so they understand what you were trying to do - not
# just the diff.
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

# Model selection is the routing contract: Runners (executables + failure
# domains), Profiles (ordered provider Candidates of runner/model/effort), and
# Routes (a Profile cascade per Purpose). The built-in defaults apply when
# 'routing' is omitted. Override individual pieces to change models, efforts, or
# runner executables.
# routing:
#   runners:
#     codex: {executable: codex, failure_domain: openai}
#     claude: {executable: claude, failure_domain: anthropic}
`

// legacyGlobalKeys maps removed global config keys to the actionable guidance
// LoadGlobal returns when one is present. Routing (runners, profiles,
// candidates, routes) is the sole model-selection contract; there are no
// aliases, compatibility spellings, or automatic rewrites.
var legacyGlobalKeys = map[string]string{
	"agent":                  "model selection is configured via `routing` (runners, profiles, routes); there is no single-agent selector",
	"fallback_agents":        "provider fail-over is configured through routing profile candidates, not a fallback-agent list",
	"acpx_path":              "acp agents were removed; declare runners under `routing.runners`",
	"acp_registry_overrides": "acp agents were removed; declare runners under `routing.runners`",
	"agent_path_override":    "runner executables are configured via `routing.runners.<name>.executable`",
	"agent_args_override":    "native agent arguments are derived from routing profile candidates and cannot be overridden",
	"auto_fix":               "per-step numeric auto-fix limits were removed; repair escalates through the routing cascade",
}

// legacyGlobalKeyOrder lists legacy keys in a stable order so a config with
// several removed keys reports a deterministic error.
var legacyGlobalKeyOrder = []string{
	"acp_registry_overrides",
	"acpx_path",
	"agent",
	"agent_args_override",
	"agent_path_override",
	"auto_fix",
	"fallback_agents",
}

// rejectLegacyGlobalKeys fails closed with actionable guidance when the global
// config still carries a removed model-selection key, instead of silently
// ignoring it or rewriting it to routing.
func rejectLegacyGlobalKeys(data []byte) error {
	var top map[string]yaml.Node
	if err := yaml.Unmarshal(data, &top); err != nil {
		// A malformed document is reported by the typed decode that follows.
		return nil
	}
	for _, key := range legacyGlobalKeyOrder {
		if _, present := top[key]; present {
			return fmt.Errorf("global config key %q is no longer supported: %s", key, legacyGlobalKeys[key])
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
		CITimeout:            DefaultCITimeout,
		StepQuietWarning:     DefaultStepQuietWarning,
		DaemonConnectTimeout: DefaultDaemonConnectTimeout,
		LogLevel:             "info",
		SessionReuse:         true,
		Routing:              DefaultRoutingConfig(),
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

	if err := rejectLegacyGlobalKeys(data); err != nil {
		return nil, err
	}

	var raw globalConfigRaw
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse global config: %w", err)
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
	cfg.Intent = raw.Intent
	cfg.Test = raw.Test
	if raw.Routing == nil && globalConfigHasKey(data, "routing") {
		// A present-but-null routing block (routing: / routing: null / ~) is an
		// explicit, incomplete replacement, not an "unset" default.
		return nil, fmt.Errorf("global routing: block is present but empty")
	}
	if raw.Routing != nil {
		raw.Routing.normalizeProfileNames()
		if err := raw.Routing.Validate(); err != nil {
			return nil, fmt.Errorf("global routing: %w", err)
		}
		cfg.Routing = *raw.Routing
	}

	return cfg, nil
}

// globalConfigHasKey reports whether the raw global config YAML contains a
// given top-level key, so an explicitly present but null block is
// distinguishable from an absent one.
func globalConfigHasKey(data []byte, key string) bool {
	var probe map[string]yaml.Node
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return false
	}
	_, ok := probe[key]
	return ok
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
	if err := rejectRepoExecutionMechanics(data); err != nil {
		return nil, err
	}
	cfg := &RepoConfig{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse repo config: %w", err)
	}
	return cfg, nil
}

// rejectRepoExecutionMechanics fails when a repository's .no-mistakes.yaml tries
// to select an agent or define model-selection execution mechanics.
// Repositories may only map purposes to existing global profiles via 'routes';
// runners, profiles, candidates, and agent selection are owned exclusively by
// global configuration.
func rejectRepoExecutionMechanics(data []byte) error {
	var probe map[string]yaml.Node
	if err := yaml.Unmarshal(data, &probe); err != nil {
		// Malformed YAML is reported by the typed decode that follows.
		return nil
	}
	guidance := map[string]string{
		"agent":      "model selection is global-only through the routing contract; a repository cannot select an agent",
		"auto_fix":   "per-step numeric auto-fix limits were removed; repair escalates through the routing cascade",
		"candidates": "candidates are owned exclusively by global configuration",
		"profiles":   "profiles are owned exclusively by global configuration",
		"routing":    "repositories may only set 'routes' mapping purposes to existing global profiles",
		"runners":    "runners are owned exclusively by global configuration",
	}
	for _, key := range []string{"agent", "auto_fix", "candidates", "profiles", "routing", "runners"} {
		if _, ok := probe[key]; ok {
			return fmt.Errorf("repo config may not define %q: %s", key, guidance[key])
		}
	}
	return nil
}

// EffectiveRepoConfig returns the repo config that should drive the pipeline
// given a pushed-branch copy and the trusted default-branch copy.
//
// The code-executing selection field Commands (run verbatim via sh -c on the
// daemon host) is taken only from the trusted copy when it is present, so a
// contributor's pushed branch cannot inject shell. Routes and Document (the
// documentation placement policy injected into the document gate prompt) are
// always trusted-only: a pushed branch cannot select its own model route or
// weaken the documentation rules that gate itself. When allowRepoCommands is
// true the maintainer has explicitly opted in (via allow_repo_commands on the
// TRUSTED default-branch copy) to honoring pushed-branch commands.
// When there is no trusted copy and the maintainer has not opted in, Commands
// is forced empty (yielding built-in defaults) rather than falling back to the
// pushed branch - this blocks the supply-chain vector for repos that ship
// .no-mistakes.yaml only on feature branches.
//
// Routes come only from the trusted default-branch copy. Non-executing fields
// (ignore patterns, intent, test) are always taken from the pushed copy since
// they cannot run arbitrary shell or select a process.
func EffectiveRepoConfig(pushed, trusted *RepoConfig, allowRepoCommands bool) *RepoConfig {
	if pushed == nil {
		pushed = &RepoConfig{}
	}
	effective := *pushed
	// Routes and documentation ownership come only from the trusted
	// default-branch copy, never the pushed branch, and are never influenced
	// by the allow_repo_commands opt-in.
	effective.Routes = nil
	effective.Document = DocumentRaw{}
	if trusted != nil {
		effective.Routes = trusted.Routes
		effective.Document = trusted.Document
	}
	if allowRepoCommands {
		return &effective
	}
	if trusted != nil {
		effective.Commands = trusted.Commands
	} else {
		effective.Commands = Commands{}
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

// Merge combines global and per-repo configuration. Commands, ignore patterns,
// and route overrides come from repo config; model selection is the global
// routing contract with any trusted repository route overrides applied.
func Merge(global *GlobalConfig, repo *RepoConfig) *Config {
	intent := intentDefaults()
	applyIntentOverrides(&intent, &global.Intent)
	applyIntentOverrides(&intent, &repo.Intent)

	test := testDefaults()
	applyTestOverrides(&test, &global.Test)
	applyTestOverrides(&test, &repo.Test)

	cfg := &Config{
		CITimeout:            global.CITimeout,
		StepQuietWarning:     global.StepQuietWarning,
		DaemonConnectTimeout: global.DaemonConnectTimeout,
		LogLevel:             global.LogLevel,
		SessionReuse:         global.SessionReuse,
		Commands:             repo.Commands,
		IgnorePatterns:       repo.IgnorePatterns,
		Intent:               intent,
		Test:                 test,
		Document:             Document{Instructions: strings.TrimSpace(repo.Document.Instructions)},
	}
	routing := global.Routing
	if routing.IsZero() {
		routing = DefaultRoutingConfig()
	}
	routing = routing.clone()
	for purpose, profile := range repo.Routes {
		routing.Routes[purpose] = Route{profile}
	}
	cfg.Routing = routing

	return cfg
}

// ValidateRouting checks the resolved routing contract, including any trusted
// repository route overrides applied during Merge. It fails closed so a
// misconfigured routing block or override never reaches model launch.
func (c *Config) ValidateRouting() error {
	return c.Routing.Validate()
}
