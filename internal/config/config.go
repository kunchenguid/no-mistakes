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
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/evidence"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
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
	// DefaultAgentTimeout bounds one pipeline agent invocation that does not
	// install a more specific deadline, so a stalled agent cannot leave a run
	// active forever. Review and Test keep their own knobs; this is the
	// default-by-construction budget for every other step.
	DefaultAgentTimeout = 30 * time.Minute
	// DefaultReviewAgentTimeout bounds one review round, including its optional
	// review-fix and rereview turns, so a stalled agent cannot leave a run
	// active forever.
	DefaultReviewAgentTimeout = 30 * time.Minute
	// DefaultTestAgentTimeout bounds one Test-step agent invocation, including
	// the post-test evidence-gathering turn and a Test-repair turn, so a stalled
	// agent cannot leave a run active forever.
	DefaultTestAgentTimeout = 30 * time.Minute
	// DefaultDaemonConnectTimeout bounds client IPC connection attempts to a
	// daemon socket that exists but is not accepting connections.
	DefaultDaemonConnectTimeout = 3 * time.Second
	// DefaultBranchSyncRemoteTimeout bounds each remote Git operation (ls-remote, fetch) in internal/branchsync. Global-config-only; a pushed branch cannot change it. Timeout still fails closed.
	DefaultBranchSyncRemoteTimeout = 60 * time.Second
	// DefaultGateReconcileInterval is how often a parked approval gate is
	// rechecked. Global-config-only; a pushed branch cannot change it.
	DefaultGateReconcileInterval = 2 * time.Minute
	// DefaultGateReconcileTimeout is the deadline for one approval-gate
	// reconciliation check (including host.Available / gh auth status).
	// Global-config-only; a pushed branch cannot change it.
	DefaultGateReconcileTimeout = 30 * time.Second
	// CITimeoutUnlimited is the sentinel meaning "monitor until the PR is
	// merged, closed, or the run is aborted - never self-terminate".
	// Any non-positive ci_timeout, or the keywords "unlimited", "none",
	// "off", and "never", resolves to this.
	CITimeoutUnlimited = time.Duration(-1)
	// DefaultCIRerunTransient is the per-check rerun budget the CI step uses
	// when ci.rerun_transient is unset. It is 0 because GitHub's CANCELLED
	// conclusion does not carry a cause: the same value covers a provider
	// aborting its own infrastructure, a maintainer stopping a runaway or
	// unsafe job, and repository concurrency with cancel-in-progress. Until a
	// reliable cause signal exists, restarting on that ambiguity risks
	// re-running work a person deliberately stopped, so rerunning cancelled
	// checks is an explicit opt-in rather than a default.
	DefaultCIRerunTransient = 0
	// MaxCIRerunTransient caps ci.rerun_transient. Reruns are cheap compared
	// with an agent round, but they are not free: each one keeps the monitor
	// polling the same commit, so the budget stays small by construction.
	MaxCIRerunTransient = 5
	// DefaultCIRevalidateRepairs is the policy the CI step uses when
	// ci.revalidate_repairs is unset. It is false because restarting the whole
	// pipeline at Review for every CI repair is the single most expensive
	// thing the pipeline can do to a run: it replays Review, Test, Document,
	// Lint, Push, and PR against the repaired head, so one repair costs
	// another full agent pass over the whole change. VISION.md's cost
	// constraint makes that opt-in.
	//
	// False does not mean "always publish". It means "publish when it is
	// provably safe to": a repair is published only when its head is the run's
	// review-approved commit or a descendant of it, and any repair that cannot
	// show that - every merge-conflict repair, since a rebase rewrites the
	// head - revalidates from Review instead. See CI.RevalidateRepairs.
	DefaultCIRevalidateRepairs = false
	// DefaultEvalMaxCases caps the auto-captured local eval corpus. Cases
	// share one object pool per repository, so the marginal cost of a case is
	// its JSON records plus the objects its commits actually introduced, not a
	// copy of the repository. The cap exists to bound that JSON and to keep
	// the corpus a recent, representative window rather than an archive.
	DefaultEvalMaxCases = 200
	// DefaultEvalDiversifiedSize caps the official gold-only eval set.
	// 0 means one gold case per stratum with no Hamilton bound.
	DefaultEvalDiversifiedSize = 32
	// DefaultEvidenceRetention is how long a run's on-disk evidence survives
	// before the daemon reaps it. It is comfortably longer than typical PR
	// review latency because a PR body references these artifacts by local path
	// whenever publishing is off or the provider has no derivable links. This
	// is no-mistakes' own budget: the point of owning it is that no OS temp
	// timer decides when a user's screenshots disappear.
	DefaultEvidenceRetention = 14 * 24 * time.Hour
	// DefaultEvidenceMaxRuns caps how many run directories survive regardless
	// of age, so a burst of parallel runs that all land inside the retention
	// window still cannot grow the directory without bound.
	DefaultEvidenceMaxRuns = 200
)

// GlobalConfig represents ~/.no-mistakes/config.yaml.
type GlobalConfig struct {
	SourceYAML           []byte              `yaml:"-"`
	Agent                types.AgentName     `yaml:"agent"`
	Agents               []types.AgentName   `yaml:"-"`
	ACPXPath             string              `yaml:"acpx_path"`
	ForgejoAXIPath       string              `yaml:"forgejo_axi_path"`
	ACPRegistryOverrides map[string]string   `yaml:"acp_registry_overrides"`
	AgentPathOverride    map[string]string   `yaml:"agent_path_override"`
	AgentArgsOverride    map[string][]string `yaml:"agent_args_override"`
	// AgentConfig is the harness-neutral per-agent tuning map (agent_config):
	// model and reasoning effort stated once in a common spelling, mapped down
	// to each harness's own mechanism by internal/agentcfg. It is additive to
	// agent_args_override, which still wins for any knob it already pins
	// natively, so every configuration written before this field keeps its exact
	// previous behavior. Global-only for the same reason as
	// agent_args_override: it describes this machine's agent setup and decides
	// which model runs with the operator's credentials, so no pushed branch may
	// set it.
	AgentConfig map[string]agentcfg.Profile `yaml:"agent_config"`
	// WorktreeRoots places a repository's pipeline run worktrees under a
	// directory the operator chose instead of the default
	// <NM_HOME>/worktrees/<repoID>. Keys are registered checkout paths
	// (Repo.WorkingPath), values are absolute directories. It exists for
	// directory-scoped toolchain configuration (mise, direnv), which resolves
	// by path ancestry and therefore never reaches a worktree under NM_HOME.
	// Placement is resolved for every consumer in internal/worktrees.
	WorktreeRoots           map[string]string `yaml:"worktree_roots"`
	CITimeout               time.Duration     `yaml:"-"`
	StepQuietWarning        time.Duration     `yaml:"-"`
	AgentTimeout            time.Duration     `yaml:"-"`
	ReviewAgentTimeout      time.Duration     `yaml:"-"`
	TestAgentTimeout        time.Duration     `yaml:"-"`
	DaemonConnectTimeout    time.Duration     `yaml:"-"`
	BranchSyncRemoteTimeout time.Duration     `yaml:"-"`
	// GateReconcileInterval / GateReconcileTimeout bound how often and how
	// long a parked approval gate is rechecked. They are machine-local
	// operator knobs (slow hosts, contended gh auth) and global-only so a
	// pushed branch cannot widen or shrink the reconcile budget.
	GateReconcileInterval time.Duration `yaml:"-"`
	GateReconcileTimeout  time.Duration `yaml:"-"`
	LogLevel              string        `yaml:"log_level"`
	// SessionReuse controls per-run agent session reuse in the review loop:
	// one durable fixer session across review-fix turns. Review turns always
	// run session-free so the rereview never resumes the session whose
	// findings prescribed the fixes it certifies. Default true; set
	// session_reuse: false to force every invocation cold.
	SessionReuse  bool          `yaml:"-"`
	ForgeProfiles ForgeProfiles `yaml:"forge_profiles"`
	AutoFix       AutoFixRaw
	// CI is the operator's own CI-step floor. It is the only place the rerun
	// budget can be set for a repository whose default branch this machine's
	// user does not control (the common case when contributing to someone
	// else's project), and a trusted repo value still wins over it.
	CI     CIRaw
	Commit CommitRaw
	Intent IntentRaw
	Test   TestRaw
	// Eval is resolved at load time because it is global-only: it describes
	// this machine's local eval corpus (disk, retention, whether review rounds
	// record replay provenance), never a repository policy. Keeping it out of
	// RepoConfig means no pushed branch can enable, disable, or resize it.
	Eval      Eval
	Providers ProvidersRaw
}

// globalConfigRaw is the on-disk YAML representation with duration as string.
type globalConfigRaw struct {
	Agent                   agentList                  `yaml:"agent"`
	ACPXPath                string                     `yaml:"acpx_path"`
	ForgejoAXIPath          string                     `yaml:"forgejo_axi_path"`
	ACPRegistryOverrides    map[string]string          `yaml:"acp_registry_overrides"`
	AgentPathOverride       map[string]string          `yaml:"agent_path_override"`
	AgentArgsOverride       map[string][]string        `yaml:"agent_args_override"`
	AgentConfig             map[string]agentProfileRaw `yaml:"agent_config"`
	WorktreeRoots           map[string]string          `yaml:"worktree_roots"`
	CITimeout               string                     `yaml:"ci_timeout"`
	DaemonConnectTimeout    string                     `yaml:"daemon_connect_timeout"`
	BranchSyncRemoteTimeout string                     `yaml:"branch_sync_remote_timeout"`
	GateReconcileInterval   string                     `yaml:"gate_reconcile_interval"`
	GateReconcileTimeout    string                     `yaml:"gate_reconcile_timeout"`
	BabysitTimeout          string                     `yaml:"babysit_timeout"`
	StepQuietWarning        string                     `yaml:"step_quiet_warning"`
	AgentTimeout            string                     `yaml:"agent_timeout"`
	ReviewAgentTimeout      string                     `yaml:"review_agent_timeout"`
	TestAgentTimeout        string                     `yaml:"test_agent_timeout"`
	LogLevel                string                     `yaml:"log_level"`
	SessionReuse            *bool                      `yaml:"session_reuse"`
	AutoFix                 AutoFixRaw                 `yaml:"auto_fix"`
	CI                      CIRaw                      `yaml:"ci"`
	Commit                  CommitRaw                  `yaml:"commit"`
	Intent                  IntentRaw                  `yaml:"intent"`
	Test                    TestRaw                    `yaml:"test"`
	Eval                    EvalRaw                    `yaml:"eval"`
	ForgeProfiles           ForgeProfiles              `yaml:"forge_profiles"`
	Providers               ProvidersRaw               `yaml:"providers"`
}

// ForgeProfile selects one isolated provider CLI configuration directory.
// ExpectedLogin optionally pins the account the profile must be signed in as;
// resolution fails closed when the profile's active login differs. It carries
// an account name only, never credentials.
type ForgeProfile struct {
	GHConfigDir   string `yaml:"gh_config_dir"`
	GLabConfigDir string `yaml:"glab_config_dir"`
	ExpectedLogin string `yaml:"expected_login"`
}

// ForgeProfiles maps a remote host token to its machine-local provider profile.
type ForgeProfiles map[string]ForgeProfile

// RepoConfig represents .no-mistakes.yaml in a repo root.
type RepoConfig struct {
	Agent          types.AgentName   `yaml:"agent"`
	Agents         []types.AgentName `yaml:"-"`
	Commands       Commands          `yaml:"commands"`
	IgnorePatterns []string          `yaml:"ignore_patterns"`
	// ProtectedPaths prevents automatic staging of dirty matching paths. It is
	// trusted-only, regardless of allow_repo_commands, so a pushed branch cannot
	// remove the maintainer's protection from its own fixes.
	ProtectedPaths []string `yaml:"protected_paths"`
	// AllowRepoCommands opts in to honoring the code-executing selection
	// fields (commands.{test,lint,format} and agent) from a contributor's
	// pushed branch instead of the trusted default-branch copy. It is read
	// ONLY from the trusted default-branch copy of .no-mistakes.yaml (never
	// the pushed SHA), so a contributor cannot self-enable. Default false:
	// the pushed branch controls nothing that executes.
	AllowRepoCommands bool `yaml:"allow_repo_commands"`
	// PR carries pull-request routing settings. BaseBranch controls where a PR
	// lands, so EffectiveRepoConfig treats it as trusted-only unless the
	// repository explicitly opts into pushed settings.
	AutoFix AutoFixRaw `yaml:"auto_fix"`
	CI      CIRaw      `yaml:"ci"`
	Commit  CommitRaw  `yaml:"commit"`
	Intent  IntentRaw  `yaml:"intent"`
	Test    TestRaw    `yaml:"test"`
	PR      PRRaw      `yaml:"pr"`
	// Providers carries provider-specific settings. Repo values overlay the
	// global ones field by field. Every field is opt-in and defaults false, and
	// none of them gates or weakens a pipeline step, so unlike the trusted-only
	// fields below they are read from the pushed branch.
	Providers ProvidersRaw `yaml:"providers"`
	// Document carries the repository's documentation placement policy. It
	// steers the document step's gate prompt, so it is honored ONLY from the
	// trusted default-branch copy of .no-mistakes.yaml (see
	// EffectiveRepoConfig): a contributor's pushed branch must not be able to
	// weaken documentation rules for its own review.
	Document DocumentRaw `yaml:"document"`
	// Review carries the repository's review-step settings. Its
	// path_instructions steer the review gate prompt, so they are honored
	// ONLY from the trusted default-branch copy of .no-mistakes.yaml (see
	// EffectiveRepoConfig), regardless of allow_repo_commands: a contributor's
	// pushed branch must not be able to inject or weaken the guidance that
	// reviews it.
	Review ReviewRaw `yaml:"review"`
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
	// NoCI declares that this repository intentionally has no CI. When true and
	// the forge reports zero checks, the CI monitor treats that empty result as
	// all-checks-passed. It is a readiness boundary honored ONLY from the trusted
	// default-branch copy of .no-mistakes.yaml (see EffectiveRepoConfig): a
	// contributor's pushed branch must not self-declare no-CI and bypass checks.
	// Default false - absence means CI is expected, and an unproven empty check
	// list remains not-ready regardless of elapsed time. If checks still appear,
	// their actual states are processed normally; the declaration never waives a
	// registered pending or failing check. No inference from workflow files,
	// prior history, branch names, or grace-period expiry.
	NoCI bool `yaml:"no_ci"`
}

// DocumentRaw is the YAML representation of document-step settings.
type DocumentRaw struct {
	// Instructions augment (never replace) the built-in documentation
	// placement policy with the repository's ownership map or extra
	// placement rules.
	Instructions string `yaml:"instructions"`
}

// ReviewRaw is the YAML representation of review-step settings.
type ReviewRaw struct {
	// PathInstructions scope extra review guidance to the paths a change
	// actually touches. The review step appends the blocks whose glob matches
	// at least one changed file; a run that touches nothing matching leaves
	// the review prompt exactly as it is without this setting.
	PathInstructions []PathInstruction `yaml:"path_instructions"`
}

// PRRaw is the YAML representation of pull-request settings.
type PRRaw struct {
	// BaseBranch selects the forge branch a PR targets. It is gate-control
	// configuration: the trusted default-branch copy wins unless the
	// repository explicitly opts into pushed-branch settings with
	// allow_repo_commands.
	BaseBranch string `yaml:"base_branch"`
}

// PathInstruction is one glob-scoped block of review guidance. Path follows the
// same match rules as ignore_patterns: no slash matches by basename, a trailing
// "/**" matches an entire subtree, and anything else is a full-path glob.
type PathInstruction struct {
	Path         string `yaml:"path"`
	Instructions string `yaml:"instructions"`
}

// Review-prompt block frame for review.path_instructions.
//
// The review step renders every matched entry as
//
//	path: <path>
//	matched files: <files>
//	instructions:
//	<instructions>
//
// so each rule travels with the scope it was selected for and no block can read
// as a global instruction. The labels live here rather than in the review step
// because the byte accounting below has to measure the real assembled section,
// not an estimate of it; internal/pipeline/steps builds its blocks from these
// same constants and TestReviewPathInstructionsSectionStaysWithinAccountedBytes
// is the drift check.
const (
	ReviewPathInstructionsHeading    = "Repository review instructions for the changed paths (trusted, from the default branch). Each block below applies only to the files listed under its path, and adds to the requirements above:"
	ReviewPathInstructionsPathLabel  = "path: "
	ReviewPathInstructionsFilesLabel = "matched files: "
	ReviewPathInstructionsRulesLabel = "instructions:"
	// ReviewPathInstructionsMaxFilesBytes bounds the matched-file list a single
	// block may print. A broad glob can match hundreds of files, so the review
	// step truncates the list deterministically and states the remaining count;
	// the accounting charges every entry this full allowance so the cap holds
	// for any diff rather than only for small ones.
	ReviewPathInstructionsMaxFilesBytes = 192
)

// Bounds on review.path_instructions.
//
// The injected text lands in the review prompt, which is already the largest
// gate prompt no-mistakes builds, and an oversized prompt fails the agent
// invocation outright instead of degrading. The budget is therefore validated
// when the config is parsed - before a run starts - rather than truncated
// silently at review time.
const (
	// MaxReviewPathInstructions is the largest number of path_instructions
	// entries a repository may configure.
	MaxReviewPathInstructions = 32
	// MaxReviewPathInstructionsBytes is the largest review-prompt section
	// path_instructions may produce, measured by ReviewPathInstructionsBytes.
	// It leaves room for the entry cap to be reached with a rule of ordinary
	// length, so neither cap makes the other unusable.
	MaxReviewPathInstructionsBytes = 16384
)

// ReviewPathInstructionsBytes returns the largest review-prompt section these
// entries can produce: the leading blank line, the heading, and for every entry
// its labels, its path, its instructions, its full matched-file allowance, and
// the separator before it. Instruction text can only shrink on its way into the
// prompt (conflict markers are removed and whitespace is collapsed), and the
// matched-file list is truncated to its allowance, so the result is an upper
// bound on the real section for any diff.
func ReviewPathInstructionsBytes(entries []PathInstruction) int {
	if len(entries) == 0 {
		return 0
	}
	total := len("\n\n") + len(ReviewPathInstructionsHeading) + len("\n")
	for i, entry := range entries {
		if i > 0 {
			total += len("\n\n")
		}
		total += len(ReviewPathInstructionsPathLabel) + len(strings.TrimSpace(entry.Path)) + len("\n")
		total += len(ReviewPathInstructionsFilesLabel) + ReviewPathInstructionsMaxFilesBytes + len("\n")
		total += len(ReviewPathInstructionsRulesLabel) + len("\n")
		total += len(strings.TrimSpace(entry.Instructions))
	}
	return total
}

// promptConflictMarkers are the merge-conflict tokens the pipeline removes from
// maintainer-authored text before injecting it into an agent prompt
// (sanitizePromptMultilineText in internal/pipeline/steps owns the removal, and
// document.instructions goes through the same path). Validation applies the same
// removal so a value that would reach the reviewer as an empty block is rejected
// here instead of disappearing from the prompt without a word.
var promptConflictMarkers = strings.NewReplacer("<<<<<<<", " ", "=======", " ", ">>>>>>>", " ")

// RenderedInstructions is the emptiness-agreement helper for instruction text,
// not a second copy of the prompt renderer. The real renderer is
// sanitizePromptMultilineText in internal/pipeline/steps, which additionally
// normalizes CR and collapses each line's runs of whitespace; internal/config
// cannot import that package, which is why the conflict-marker replacer above is
// duplicated here at all. Two invariants tie the two together, and the rest of
// this feature silently depends on both:
//
//   - Emptiness agrees exactly. This returns "" for precisely the inputs the
//     prompt renderer reduces to "", so validation can reject a value that would
//     otherwise reach the reviewer as an empty block.
//   - The prompt renderer never lengthens text, so the rendered instructions are
//     no longer than strings.TrimSpace of the raw value and
//     ReviewPathInstructionsBytes stays an upper bound on the assembled section.
//
// A change to sanitizePromptMultilineText that can lengthen text (escaping,
// wrapping) or that strips a token this replacer keeps breaks one of them;
// TestPathInstructionRenderingAgreesWithConfigValidation is the drift check.
func RenderedInstructions(instructions string) string {
	return strings.TrimSpace(promptConflictMarkers.Replace(instructions))
}

func (c *RepoConfig) UnmarshalYAML(value *yaml.Node) error {
	type repoConfigRaw struct {
		Agent                  agentList    `yaml:"agent"`
		Commands               Commands     `yaml:"commands"`
		IgnorePatterns         []string     `yaml:"ignore_patterns"`
		ProtectedPaths         []string     `yaml:"protected_paths"`
		AllowRepoCommands      bool         `yaml:"allow_repo_commands"`
		AutoFix                AutoFixRaw   `yaml:"auto_fix"`
		CI                     CIRaw        `yaml:"ci"`
		Commit                 CommitRaw    `yaml:"commit"`
		Intent                 IntentRaw    `yaml:"intent"`
		Test                   TestRaw      `yaml:"test"`
		PR                     PRRaw        `yaml:"pr"`
		Document               DocumentRaw  `yaml:"document"`
		Review                 ReviewRaw    `yaml:"review"`
		DisableProjectSettings bool         `yaml:"disable_project_settings"`
		NoCI                   bool         `yaml:"no_ci"`
		Providers              ProvidersRaw `yaml:"providers"`
	}
	var raw repoConfigRaw
	if err := value.Decode(&raw); err != nil {
		return err
	}
	c.Agent = firstAgent(raw.Agent)
	c.Agents = copyAgents(raw.Agent)
	c.Commands = raw.Commands
	c.IgnorePatterns = raw.IgnorePatterns
	c.ProtectedPaths = raw.ProtectedPaths
	c.AllowRepoCommands = raw.AllowRepoCommands
	c.AutoFix = raw.AutoFix
	c.CI = raw.CI
	c.Commit = raw.Commit
	c.Intent = raw.Intent
	c.Test = raw.Test
	c.PR = raw.PR
	c.Document = raw.Document
	c.Review = raw.Review
	c.DisableProjectSettings = raw.DisableProjectSettings
	c.NoCI = raw.NoCI
	c.Providers = raw.Providers
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

// CIRaw is the YAML representation of CI-step settings.
// Pointer fields distinguish "not set" (nil) from "set to 0" (disabled).
type CIRaw struct {
	RerunTransient *int `yaml:"rerun_transient"`
	// RevalidateRepairs is a pointer so an explicit `false` in a repository's
	// config can override a global `true`, which a plain bool could not
	// express (it would be indistinguishable from "not set").
	RevalidateRepairs *bool `yaml:"revalidate_repairs"`
}

// CI holds the resolved CI-step settings.
type CI struct {
	// RerunTransient is how many times the CI step may re-run a single check
	// the provider reported as cancelled - the one terminal outcome it
	// attributes to itself rather than to the job - before that check reaches
	// an approval gate. 0 disables reruns and restores the behavior of
	// escalating every failure on sight.
	RerunTransient int
	// RevalidateRepairs selects what happens after the CI step's fix agent
	// produces a real repair commit.
	//
	// One rule decides delivery on every CI-fix path, automatic and manual, CI
	// failure and merge conflict alike: a repair is published without
	// revalidating only when its continuity with the reviewed, published head
	// can be PROVEN - the repaired head is the run's review-approved commit or
	// a descendant of it - and revalidates from Review when it cannot.
	//
	// false (default): a provable repair is published through the same guarded
	// force-push path the Push step uses - review-approved-head continuity, the
	// force-with-lease anchor, remote verification, the gate mirror, and the
	// push binding all still apply, and none of it is recorded until all of it
	// succeeds - and the CI monitor keeps watching the same run for the new
	// head. The run's review approval stays valid because the repair descends
	// from the approved head. A repair whose continuity cannot be proven takes
	// the revalidating path below instead; a merge-conflict repair always does,
	// because a rebase makes its head a non-descendant and resolving a conflict
	// changes the commit's patch-id, so no content-based guard can tell a
	// resolved rebase from one that dropped the work.
	//
	// true: the repair is kept local, the run's review approval is revoked,
	// and the pipeline restarts at Review so the repaired head re-passes
	// Review, Test, Document, and Lint before Push republishes it. Safer, and
	// materially more expensive in wall-clock time and tokens - which is why
	// it is opt-in (see VISION.md).
	RevalidateRepairs bool
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
	ReplayGlobalYAML      []byte
	ReplayRepoYAML        []byte
	TrustedConfigSHA      string
	CaptureEvalProvenance bool
	Agent                 types.AgentName
	Agents                []types.AgentName
	ACPXPath              string
	ForgejoAXIPath        string
	ACPRegistryOverrides  map[string]string
	AgentPathOverride     map[string]string
	AgentArgsOverride     map[string][]string
	AgentConfig           map[string]agentcfg.Profile
	CITimeout             time.Duration
	StepQuietWarning      time.Duration
	AgentTimeout          time.Duration
	ReviewAgentTimeout    time.Duration
	TestAgentTimeout      time.Duration
	GateReconcileInterval time.Duration
	GateReconcileTimeout  time.Duration
	LogLevel              string
	SessionReuse          bool
	Eval                  Eval
	Commands              Commands
	IgnorePatterns        []string
	ProtectedPaths        []string
	AutoFix               AutoFix
	CI                    CI
	Commit                Commit
	Intent                Intent
	Test                  Test
	Document              Document
	Review                Review
	PR                    PR
	ForgeProfiles         ForgeProfiles
	// DisableProjectSettings is the resolved, trusted-only opt-out (see the
	// RepoConfig field). When true, gate agents are launched with their
	// project-level settings/instructions suppressed; the daemon fails the run
	// closed if the resolved harness has no verified suppression knob.
	DisableProjectSettings bool
	// NoCI is the resolved, trusted-only declaration that this repository
	// intentionally has no CI (see the RepoConfig field). When true and the
	// forge reports zero checks, the CI monitor treats that as all-checks-passed.
	NoCI bool
	// Providers holds the resolved provider-specific settings.
	Providers Providers
}

// ProvidersRaw is the YAML representation of provider-specific settings,
// keyed by provider name so new providers can be added without reshaping
// existing config.
type ProvidersRaw struct {
	GitHub      GitHubProviderRaw      `yaml:"github"`
	GitLab      GitLabProviderRaw      `yaml:"gitlab"`
	Bitbucket   BitbucketProviderRaw   `yaml:"bitbucket"`
	AzureDevOps AzureDevOpsProviderRaw `yaml:"azuredevops"`
}

// GitHubProviderRaw is the YAML representation of GitHub provider settings.
// Pointer fields distinguish "not set" (nil) from an explicit false.
type GitHubProviderRaw struct {
	DraftPullRequests *bool `yaml:"draft_pull_requests"`
}

// GitLabProviderRaw is the YAML representation of GitLab provider settings.
// Pointer fields distinguish "not set" (nil) from an explicit false.
type GitLabProviderRaw struct {
	DraftPullRequests *bool `yaml:"draft_pull_requests"`
}

// BitbucketProviderRaw is the YAML representation of Bitbucket provider settings.
// Pointer fields distinguish "not set" (nil) from an explicit false.
type BitbucketProviderRaw struct {
	DraftPullRequests *bool `yaml:"draft_pull_requests"`
}

// AzureDevOpsProviderRaw is the YAML representation of Azure DevOps provider
// settings. Pointer fields distinguish "not set" (nil) from an explicit false.
type AzureDevOpsProviderRaw struct {
	DraftPullRequests *bool `yaml:"draft_pull_requests"`
}

// Providers holds resolved provider-specific settings.
type Providers struct {
	GitHub      GitHubProvider
	GitLab      GitLabProvider
	Bitbucket   BitbucketProvider
	AzureDevOps AzureDevOpsProvider
}

// GitHubProvider holds resolved GitHub provider settings.
type GitHubProvider struct {
	// DraftPullRequests opens created GitHub PRs as drafts
	// (gh pr create --draft). Default false.
	DraftPullRequests bool
}

// GitLabProvider holds resolved GitLab provider settings.
type GitLabProvider struct {
	// DraftPullRequests opens created GitLab MRs as drafts
	// (glab mr create --draft). Default false.
	DraftPullRequests bool
}

// BitbucketProvider holds resolved Bitbucket provider settings.
type BitbucketProvider struct {
	// DraftPullRequests opens created Bitbucket PRs as drafts
	// ("draft": true in the create-PR request body). Default false.
	DraftPullRequests bool
}

// AzureDevOpsProvider holds resolved Azure DevOps provider settings.
type AzureDevOpsProvider struct {
	// DraftPullRequests opens created Azure DevOps PRs as drafts
	// (az repos pr create --draft true). Default false.
	DraftPullRequests bool
}

// PR is the resolved pull-request configuration.
type PR struct {
	BaseBranch string
}

// Document is the resolved document-step config. Instructions come from the
// trusted default-branch repo config and augment the built-in placement
// policy in the document prompt.
type Document struct {
	Instructions string
}

// Review is the resolved review-step config. PathInstructions come from the
// trusted default-branch repo config and scope extra review guidance to the
// changed paths each glob matches.
type Review struct {
	PathInstructions []PathInstruction
}

// TestRaw is the YAML representation of test-step settings.
type TestRaw struct {
	Evidence EvidenceRaw `yaml:"evidence"`
}

// EvidenceRaw is the YAML representation of test-evidence settings.
// Pointer fields distinguish "not set" (nil) from explicit zero/false values.
type EvidenceRaw struct {
	StoreInRepo *bool `yaml:"store_in_repo"`
	// AttachMedia uploads image and video evidence to GitHub user-attachments
	// when the PR body is rendered. It defaults on so default-config PRs stop
	// citing local disk paths for screenshots; set false to opt out. The
	// orphan-branch store (store_in_repo) is independent: when both are on,
	// the PR body carries both the commit-pinned link and the attachment.
	// Like store_in_repo, this is pushed-readable.
	AttachMedia *bool   `yaml:"attach_media"`
	Dir         *string `yaml:"dir"`
	// Branch selects the orphan evidence branch. It names a git ref the
	// daemon pushes to with the maintainer's credentials, so it is honored
	// ONLY from the trusted default-branch copy of .no-mistakes.yaml (see
	// EffectiveRepoConfig): a contributor's pushed branch must not be able to
	// aim evidence commits at another branch of the repository.
	Branch *string `yaml:"branch"`
	// LocalRoot, Retention, and MaxRuns describe this MACHINE's evidence
	// storage: where the daemon writes artifacts on local disk and how long it
	// keeps them. They are global-only - Merge resolves them straight from
	// GlobalConfig and never from a repository, trusted copy included. A
	// repository does not get to name a filesystem path the daemon writes to,
	// nor to set the retention budget for a resource every other repository on
	// the machine shares. (Contrast Branch, which is trusted-repo-settable
	// because a branch genuinely is per-repository state.)
	//
	// LocalRoot must be absolute; see validateTestRaw.
	LocalRoot *string `yaml:"local_root"`
	Retention *string `yaml:"retention"`
	MaxRuns   *int    `yaml:"max_runs"`
}

// Test is the resolved test-step config.
type Test struct {
	Evidence Evidence
}

// Evidence is the resolved test-evidence config. When StoreInRepo is true, the
// run publishes its evidence artifacts to the orphan Branch of the same
// repository, under Dir, and links them from the pull request body. Evidence
// never enters the pushed code branch, so it never reaches the default
// branch's history. Otherwise evidence stays on local disk under LocalRoot.
// AttachMedia (default true) additionally uploads image and video artifacts to
// GitHub user-attachments at PR render time so remote reviewers can open them
// without an evidence branch. Text artifacts stay inlined or locally cited.
type Evidence struct {
	StoreInRepo bool
	AttachMedia bool
	Dir         string
	Branch      string
	// LocalRoot overrides the app-root default for on-disk evidence; empty
	// means paths.EvidenceDir(). Retention and MaxRuns bound how much of it
	// survives: no-mistakes reaps its own evidence rather than leaving that to
	// an OS temp-directory timer. Zero disables the corresponding bound.
	LocalRoot string
	Retention time.Duration
	MaxRuns   int
}

// EvalRaw is the YAML representation of local evaluation-corpus settings.
// Pointer fields distinguish "not set" (nil) from explicit zero/false values.
type EvalRaw struct {
	CaptureProvenance *bool `yaml:"capture_provenance"`
	AutoCapture       *bool `yaml:"auto_capture"`
	MaxCases          *int  `yaml:"max_cases"`
	DiversifiedSize   *int  `yaml:"diversified_size"`
}

// Eval is the resolved local evaluation-corpus config. It is deliberately a
// first-class configuration key rather than an environment variable: the
// daemon is a long-lived launchd/systemd service whose unit file is re-rendered
// on install and update, and only proxy variables survive that re-render, so an
// environment-gated corpus would silently stop collecting after an update.
//
// CaptureProvenance is the upstream half: it makes every review round record
// the exact commit and configuration inputs a replay needs. A round written
// with it off can never be captured afterwards, because the pinned global
// configuration is a point-in-time snapshot that no longer exists anywhere.
//
// AutoCapture is the downstream half: it freezes each finished run's review
// passes into the local corpus without anyone running a command. It has no
// effect while CaptureProvenance is off, since there is nothing to freeze.
type Eval struct {
	CaptureProvenance bool
	AutoCapture       bool
	// MaxCases caps the auto-captured corpus. 0 keeps every case. Pruning is
	// oldest-first and never removes a case that already has recorded
	// candidate replays, so a corpus you have spent tokens on is never
	// silently reclaimed underneath a comparison.
	MaxCases int
	// DiversifiedSize caps the official gold-only eval set. 0 means one gold
	// case per stratum (no Hamilton bound). Unlabeled cases never fill it.
	DiversifiedSize int
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

// resolvePathInstructions trims every entry and drops the ones left without a
// path or without instruction text that survives prompt rendering, so the
// resolved config never carries an entry the review step would have to skip.
// Parsing already rejects those, but Merge also runs on configs built in code.
func resolvePathInstructions(entries []PathInstruction) []PathInstruction {
	if len(entries) == 0 {
		return nil
	}
	out := make([]PathInstruction, 0, len(entries))
	for _, entry := range entries {
		trimmed := PathInstruction{
			Path:         strings.TrimSpace(entry.Path),
			Instructions: strings.TrimSpace(entry.Instructions),
		}
		if trimmed.Path == "" || RenderedInstructions(trimmed.Instructions) == "" {
			continue
		}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// defaultConfigYAML is the template written when no global config file exists.
const defaultConfigYAML = `# no-mistakes global configuration

# Agent to use for code generation. This may also be an ordered fallback list,
# for example: agent: [codex, grok]
# Options: auto, claude, codex, grok, rovodev, opencode, pi, copilot, cursor, acp:<target>
# "auto" detects the first available native agent or ACP alias on your system
# "cursor" is an ACP alias for acp:cursor using cursor-agent acp via acpx
# "acp:cursor" also uses that Cursor default command
# Use acp:<target> to run an optional user-installed acpx target, for example acp:gemini
agent: auto

# Optional path to the user-installed acpx binary for acp:<target> agents and ACP aliases
# acpx_path: acpx

# forgejo-axi executable used for Forgejo provider operations
forgejo_axi_path: forgejo-axi

# Optional ACP target command overrides for acp:<target> agents and ACP aliases
# acp_registry_overrides:
#   local-gemini: node /opt/mock-acp-agent.mjs
#   cursor: cursor-agent acp

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

# Maximum wall-clock time for one pipeline agent invocation that does not
# install a more specific deadline (document, lint, rebase, PR, CI-fix, and
# auto-fix). A stalled agent fails the run instead of leaving it active.
agent_timeout: "30m"

# Maximum wall-clock time for one review round, including its optional
# review-fix and rereview turns. A stalled review agent fails the run instead
# of leaving it active.
review_agent_timeout: "30m"

# Maximum wall-clock time for one Test-step agent invocation, including the
# post-test evidence-gathering turn. A stalled test agent fails the run instead
# of leaving it active.
test_agent_timeout: "30m"

# Maximum time a CLI client waits for an existing daemon socket to accept a
# connection before failing instead of hanging.
daemon_connect_timeout: "3s"

# Maximum time guarded branch synchronization waits for one remote Git operation
# (ls-remote or fetch) before treating the target as offline. Global-only.
branch_sync_remote_timeout: "60s"

# How often a parked approval gate is rechecked, and the deadline for each
# check (including gh auth status). Raise gate_reconcile_timeout on a slow or
# contended machine so a transient auth-status delay is not cancelled mid-call.
# Global-only.
gate_reconcile_interval: "2m"
gate_reconcile_timeout: "30s"

# Reuse one durable fixer session per run across review-fix turns. Review turns
# always run session-free so a rereview never resumes the session that prescribed
# its fixes. Supported for claude, codex, grok, and pi; other agents run cold.
# Set false to force every agent invocation cold.
session_reuse: true

# Log level for daemon output
# Options: debug, info, warn, error
log_level: info

# Override native agent binary paths (optional)
# agent_path_override:
#   claude: /usr/local/bin/claude
#   codex: /opt/codex
#   grok: /Users/you/.grok/bin/grok

# Model and reasoning effort per agent, in one common spelling (optional, global
# only). no-mistakes maps these down to whatever the harness actually uses:
# --model/--effort for claude and copilot, -m plus -c model_reasoning_effort for
# codex, --model/--reasoning-effort for grok, --model/--thinking for pi, the
# session-message body for opencode (its model needs the provider/model form),
# and acpx --model for cursor and acp:<target>. Effort is one of
# minimal, low, medium, high, xhigh, max; a harness rejects any level it does not
# implement. rovodev and antigravity expose no mechanism no-mistakes can set, so
# agent_config is refused for them; agent_args_override remains an escape hatch
# only if your installed CLI build accepts a suitable flag.
# agent_config:
#   codex:
#     model: gpt-5.4
#     effort: low
#   claude:
#     model: sonnet
#     effort: high
#   opencode:
#     model: openai/gpt-5
#
# Extra native agent CLI flags (optional, global only)
# Codex service_tier controls speed/priority; model_reasoning_effort controls reasoning depth.
# A flag here always wins over the same knob in agent_config, so an existing
# override keeps its exact behavior.
# agent_args_override:
#   codex:
#     - -m
#     - gpt-5.4
#     - -c
#     - service_tier="priority"
#     - -c
#     - model_reasoning_effort="low"
#
# Where a repository's pipeline run worktrees are created (optional). By
# default they live under <NM_HOME>/worktrees/<repo id>, which inherits no
# directory-scoped toolchain configuration. Point a checkout at a directory of
# your own and its runs are created there instead, one directory per run, so
# mise/direnv settings on that directory reach every run. Keys are the checkout
# paths you ran "no-mistakes init" in, values must be absolute directories.
# Only the directories no-mistakes' own run records name are ever created,
# cleaned up, or removed there; everything else, including a directory that
# merely looks like a run worktree, is left alone. Each checkout needs its own
# root, and it must be outside NM_HOME and outside every checkout.
# worktree_roots:
#   /Users/you/src/my-repo: /Users/you/work/my-repo-runs

# Maximum follow-up auto-fix attempts per step (0 = disabled after the initial pass)
# Document fixes are attempted during the initial document pass.
auto_fix:
  rebase: 3
  lint: 3
  test: 3
  review: 0
  document: 3
  ci: 3

# How many times the CI step may re-run a single check the provider reported as
# cancelled before that check reaches an approval gate instead of the fix agent.
# Defaults to 0: a cancelled conclusion does not identify who cancelled, so a
# rerun can restart a job a maintainer or a concurrency rule stopped on purpose.
# Raise this only for repositories whose cancellations are known to be
# provider-side. Each rerun is another workflow run billed to the repository
# being contributed to. A repository that sets ci.rerun_transient on its own
# default branch overrides this value.
ci:
  rerun_transient: 0
  # Whether EVERY CI repair must re-pass the whole pipeline before it is
  # published, or only the ones whose continuity with the reviewed head cannot
  # be proven. Defaults to false: a repair that descends from the reviewed head
  # is published through the same guarded force-push path the Push step uses and
  # CI keeps monitoring, so one repair costs one agent round. A repair that
  # cannot show that ancestry revalidates from Review anyway - a merge-conflict
  # repair always does, because rebasing rewrites the head. Set true to restart
  # validation at Review for every repair - safer, and it pays for another full
  # pipeline pass in wall clock and tokens every time CI is repaired. A
  # repository that sets ci.revalidate_repairs on its own default branch
  # overrides this value.
  revalidate_repairs: false

# Auto-fix commit subject template. Available variables: {{.Step}} and {{.Summary}}.
# Repo config may override this value.
# commit:
#   fix_message: "no-mistakes({{.Step}}): {{.Summary}}"

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
# gathers to demonstrate the change works). By default they are kept on local
# disk under <NM_HOME>/evidence. attach_media (default true) uploads image and
# video artifacts to GitHub user-attachments when the PR is rendered so remote
# reviewers can open them; text artifacts stay inlined. Opt in to
# store_in_repo to also publish the full directory to an orphan evidence branch
# in the same repository and link it from the PR body. The evidence branch
# shares no history with your code branches, so artifacts never enter the
# pushed branch or the default branch. When both are on, the PR body carries
# both the attachment and the commit-pinned link.
#
# no-mistakes reaps its own evidence rather than leaving that to an OS temp
# directory timer: retention ages run directories out (default 14 days) and
# max_runs caps how many survive regardless of age (default 200). Set retention
# to "unlimited", or either to 0, to disable that bound. local_root moves the
# directory to another disk and must be an absolute path. These three are
# global-only - a repository's .no-mistakes.yaml cannot change where this
# machine writes evidence or how long it keeps it.
# test:
#   evidence:
#     store_in_repo: true
#     attach_media: true
#     dir: .no-mistakes/evidence
#     branch: no-mistakes/evidence
#     local_root: /var/lib/no-mistakes/evidence
#     retention: 720h
#     max_runs: 50

# Local review evaluation corpus, used by "no-mistakes eval" to compare
# agent candidates, pinned to an explicit model and reasoning effort, against
# review passes your own pipeline already made.
# capture_provenance records, on every review round, the exact commits and
# configuration a replay needs; it cannot be added afterwards, so a round
# recorded without it is never replayable. auto_capture freezes each finished
# run's review passes into the corpus so it fills without anyone remembering to
# collect it. Cases of the same repository share one local object pool, so a
# case costs its own records plus the objects its commits introduced - not a
# copy of the repository. max_cases bounds the corpus: the oldest cases are
# dropped first, and a case that already has recorded replays is never dropped.
# Set max_cases to 0 to keep every case. diversified_size caps the official
# gold-only eval set (default 32); 0 means one gold case per stratum. Unlabeled
# cases never fill it. Everything stays under <NM_HOME>/eval and is never
# uploaded anywhere.
eval:
  capture_provenance: true
  auto_capture: true
  max_cases: 200
  diversified_size: 32

# Provider-specific settings. Opt in to opening created PRs/MRs as drafts.
# providers:
#   github:
#     draft_pull_requests: true
#   gitlab:
#     draft_pull_requests: true
#   bitbucket:
#     draft_pull_requests: true
#   azuredevops:
#     draft_pull_requests: true
`

// defaultBinary maps agent names to their default binary names.
var defaultBinary = map[types.AgentName]string{
	types.AgentClaude:      "claude",
	types.AgentCodex:       "codex",
	types.AgentGrok:        "grok",
	types.AgentRovoDev:     "acli",
	types.AgentOpenCode:    "opencode",
	types.AgentPi:          "pi",
	types.AgentCopilot:     "copilot",
	types.AgentAntigravity: "agy",
}

var nativeAgentProbeOrder = []types.AgentName{
	types.AgentClaude,
	types.AgentCodex,
	types.AgentGrok,
	types.AgentOpenCode,
	types.AgentRovoDev,
	types.AgentPi,
	types.AgentCopilot,
	types.AgentAntigravity,
}

func isACPAgent(name types.AgentName) bool {
	_, ok := types.ACPTargetFor(name)
	return ok
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
// explicit agent must be runnable; auto probes native agents, then ACP aliases;
// an ordered list is filtered to available agents, deduplicated by resolved
// identity, and kept as fallbacks. The lookPath function should behave like
// exec.LookPath.
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
	probed := make([]string, 0, len(nativeAgentProbeOrder)+len(types.ACPAliases())+1)
	for _, name := range nativeAgentProbeOrder {
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
	for _, alias := range types.ACPAliases() {
		available, bins, err := c.acpAvailable(alias.Name, lookPath)
		probed = append(probed, bins...)
		if err != nil {
			return "", err
		}
		if available {
			return alias.Name, nil
		}
	}
	return "", noRunnableAgentError([]types.AgentName{types.AgentAuto}, probed)
}

func (c *Config) resolveAgentList(ctx context.Context, candidates []types.AgentName, lookPath func(string) (string, error)) ([]types.AgentName, error) {
	resolved := make([]types.AgentName, 0, len(candidates))
	seen := map[string]bool{}
	probed := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		name, ok, probe, err := c.resolveConfiguredAgent(ctx, candidate, lookPath)
		if probe != "" {
			probed = append(probed, probe)
		}
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		identity := resolvedAgentIdentity(name)
		if seen[identity] {
			continue
		}
		seen[identity] = true
		resolved = append(resolved, name)
	}
	if len(resolved) == 0 {
		return nil, noRunnableAgentError(candidates, probed)
	}
	return resolved, nil
}

func resolvedAgentIdentity(name types.AgentName) string {
	if target, ok := types.ACPTargetFor(name); ok {
		return "acp:" + target
	}
	return "native:" + string(name)
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
		return "", false, string(name), fmt.Errorf("unknown agent %q; valid options: auto, claude, codex, grok, rovodev, opencode, pi, copilot, cursor, antigravity, acp:<target> (set 'agent' in ~/.no-mistakes/config.yaml)", name)
	}
	if isACPAgent(name) {
		available, bins, err := c.acpAvailable(name, lookPath)
		probe := strings.Join(bins, ", ")
		if err != nil {
			return "", false, probe, err
		}
		return name, available, probe, nil
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
// ACP agents and ACP aliases use acpx_path if set, otherwise acpx.
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

// acpAvailable reports whether the acpx shim and any probeable raw-command
// executable can be resolved. Only bare command names and clean absolute paths
// are probeable; relative, quoted, or escaped raw commands are left for acpx to
// execute from the worktree. It returns the binaries it considered for diagnostics.
func (c *Config) acpAvailable(name types.AgentName, lookPath func(string) (string, error)) (bool, []string, error) {
	bins := c.acpBinaries(name)
	for _, bin := range bins {
		if _, err := lookPath(bin); err != nil {
			if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
				return false, bins, nil
			}
			return false, bins, fmt.Errorf("resolve %s agent from %q: %w", name, bin, err)
		}
	}
	return true, bins, nil
}

func (c *Config) acpBinaries(name types.AgentName) []string {
	bins := make([]string, 0, 2)
	if target, ok := types.ACPTargetFor(name); ok {
		if bin, probeable := acpCommandBinaryForProbe(types.ACPRawCommand(target, c.ACPRegistryOverrides)); probeable {
			bins = append(bins, bin)
		}
	}
	return append(bins, c.AgentPathFor(name))
}

func acpCommandBinaryForProbe(command string) (string, bool) {
	return acpCommandBinaryForProbeForOS(command, runtime.GOOS)
}

func acpCommandBinaryForProbeForOS(command, goos string) (string, bool) {
	if strings.ContainsAny(command, `"'`) {
		return "", false
	}
	if goos != "windows" && strings.ContainsRune(command, '\\') {
		return "", false
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", false
	}
	bin := fields[0]
	if isAbsolutePathForProbe(bin, goos) {
		return bin, true
	}
	if containsPathSeparatorForProbe(bin, goos) {
		return "", false
	}
	return bin, true
}

func isAbsolutePathForProbe(path, goos string) bool {
	if goos == runtime.GOOS {
		return filepath.IsAbs(path)
	}
	if goos == "windows" {
		return isWindowsAbsolutePath(path)
	}
	return strings.HasPrefix(path, "/")
}

func isWindowsAbsolutePath(path string) bool {
	if len(path) >= 3 && isASCIILetter(path[0]) && path[1] == ':' && isWindowsPathSeparator(path[2]) {
		return true
	}
	return len(path) >= 3 && isWindowsPathSeparator(path[0]) && isWindowsPathSeparator(path[1])
}

func isASCIILetter(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func isWindowsPathSeparator(value byte) bool {
	return value == '\\' || value == '/'
}

func containsPathSeparatorForProbe(path, goos string) bool {
	if goos == "windows" {
		return strings.ContainsAny(path, `/\`)
	}
	return strings.ContainsRune(path, '/')
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

// AgentProfile returns the harness-neutral model/effort selection for the
// configured agent, as declared in agent_config. The zero Profile means the
// harness keeps its own defaults.
func (c *Config) AgentProfile() agentcfg.Profile {
	return c.AgentProfileFor(c.Agent)
}

func (c *Config) AgentProfileFor(name types.AgentName) agentcfg.Profile {
	if c.AgentConfig == nil {
		return agentcfg.Profile{}
	}
	return c.AgentConfig[string(name)]
}

// agentProfileRaw is the on-disk YAML shape of one agent_config entry. Effort
// is a string here so an invalid level is reported as a config error naming the
// valid vocabulary rather than decoding into a value no harness accepts.
type agentProfileRaw struct {
	Model  string `yaml:"model"`
	Effort string `yaml:"effort"`
}

// parseAgentConfig validates the agent_config map and resolves it to
// harness-neutral profiles. Every knob is checked against what the named
// harness can actually express, so an unmappable request fails at load rather
// than being silently dropped at run time.
func parseAgentConfig(raw map[string]agentProfileRaw) (map[string]agentcfg.Profile, error) {
	profiles := make(map[string]agentcfg.Profile, len(raw))
	for name, entry := range raw {
		agentName := types.AgentName(name)
		if !agentcfg.Known(agentName) {
			return nil, fmt.Errorf("invalid agent name in agent_config: %q (valid: %s, cursor, acp:<target>)", name, strings.Join(agentNamesText(agentcfg.Agents()), ", "))
		}
		effort, err := agentcfg.ParseEffort(entry.Effort)
		if err != nil {
			return nil, fmt.Errorf("invalid agent_config.%s: %w", name, err)
		}
		profile := agentcfg.Profile{Model: strings.TrimSpace(entry.Model), Effort: effort}
		if err := agentcfg.Validate(agentName, profile); err != nil {
			return nil, fmt.Errorf("invalid agent_config.%s: %w", name, err)
		}
		if profile.IsZero() {
			continue
		}
		profiles[name] = profile
	}
	if len(profiles) == 0 {
		return nil, nil
	}
	return profiles, nil
}

func agentNamesText(names []types.AgentName) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, string(name))
	}
	return out
}

// agentArgsOverrideAgents lists native agent names accepted as keys in
// agent_args_override.
var agentArgsOverrideAgents = map[string]bool{
	string(types.AgentClaude):      true,
	string(types.AgentCodex):       true,
	string(types.AgentGrok):        true,
	string(types.AgentRovoDev):     true,
	string(types.AgentOpenCode):    true,
	string(types.AgentPi):          true,
	string(types.AgentCopilot):     true,
	string(types.AgentAntigravity): true,
}

// reservedAgentArgs lists flags that no-mistakes manages internally and that
// users cannot override through agent_args_override. A flag is matched by its
// bare form (e.g. "--color") as well as the "--color=value" form.
var reservedAgentArgs = map[string]map[string]bool{
	string(types.AgentAntigravity): {
		"--dangerously-skip-permissions": true,
		"--print":                        true,
		"--json-schema":                  true,
		"--output-format":                true,
		"--conversation":                 true,
		"-c":                             true,
		"--continue":                     true,
	},
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
	string(types.AgentGrok): {
		"-p":                       true,
		"--single":                 true,
		"--prompt-file":            true,
		"--prompt-json":            true,
		"--output-format":          true,
		"--json-schema":            true,
		"-r":                       true,
		"--resume":                 true,
		"-c":                       true,
		"--continue":               true,
		"--fork-session":           true,
		"--session-id":             true,
		"--system-prompt-override": true,
		"--system-prompt":          true,
		"--rules":                  true,
		"--append-system-prompt":   true,
		"--agent":                  true,
		"--agents":                 true,
		"--verbatim":               true,
		"--no-subagents":           true,
		"--no-auto-update":         true,
		"--cwd":                    true,
		"--restore-code":           true,
		"--worktree":               true,
		"--worktree-ref":           true,
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
		"-c":           true,
		"--continue":   true,
		"-r":           true,
		"--resume":     true,
		"--session":    true,
		"--session-id": true,
		"--fork":       true,
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
			return fmt.Errorf("invalid agent name in agent_args_override: %q (valid: claude, codex, grok, rovodev, opencode, pi, copilot, antigravity)", name)
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

// ValidateWorktreeRoots checks a worktree_roots map before any placement is
// derived from it. Every entry must name an absolute checkout path and an
// absolute directory: a relative path would be interpreted against whatever
// working directory the daemon happens to have, so run worktrees would land
// somewhere different depending on who started it - the opposite of the
// deterministic placement the setting exists to provide.
//
// Two entries may not name the same root, and two keys may not name the same
// checkout once canonicalized (a symlink and its target, "/x" and "/x/").
// Both are rejected rather than resolved because the consequences are
// destructive, not cosmetic: cleanup and eject identify a run worktree by its
// position in a root, so two repositories sharing a root would delete each
// other's runs, and a duplicate key would pick an arbitrary winner. A root
// equal to its own checkout is rejected for the same reason - it would place
// run worktrees inside the repository they are validating.
func ValidateWorktreeRoots(roots map[string]string) error {
	owners := make(map[string]string, len(roots))
	checkouts := make(map[string]string, len(roots))
	for _, checkout := range sortedKeys(roots) {
		root := roots[checkout]
		if strings.TrimSpace(checkout) == "" {
			return fmt.Errorf("invalid worktree_roots: empty checkout path")
		}
		if !filepath.IsAbs(checkout) {
			return fmt.Errorf("invalid worktree_roots: checkout path %q is not absolute", checkout)
		}
		if strings.TrimSpace(root) == "" {
			return fmt.Errorf("invalid worktree_roots[%q]: empty worktree root", checkout)
		}
		if !filepath.IsAbs(root) {
			return fmt.Errorf("invalid worktree_roots[%q]: %q is not an absolute path", checkout, root)
		}
		canonicalCheckout := worktrees.Canonical(checkout)
		if first, dup := checkouts[canonicalCheckout]; dup {
			return fmt.Errorf("invalid worktree_roots[%q]: %q already names the same checkout", checkout, first)
		}
		checkouts[canonicalCheckout] = checkout
		canonicalRoot := worktrees.Canonical(root)
		if first, dup := owners[canonicalRoot]; dup {
			return fmt.Errorf("invalid worktree_roots[%q]: worktree root %q is already used by %q; each checkout needs its own root", checkout, root, first)
		}
		owners[canonicalRoot] = checkout
		if canonicalRoot == canonicalCheckout {
			return fmt.Errorf("invalid worktree_roots[%q]: worktree root must not be the checkout itself", checkout)
		}
	}
	return nil
}

// sortedKeys keeps validation errors deterministic: map iteration order would
// otherwise decide which of several bad entries is reported.
func sortedKeys(roots map[string]string) []string {
	keys := make([]string, 0, len(roots))
	for key := range roots {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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
		Agent:                   types.AgentAuto,
		Agents:                  []types.AgentName{types.AgentAuto},
		ForgejoAXIPath:          "forgejo-axi",
		CITimeout:               DefaultCITimeout,
		StepQuietWarning:        DefaultStepQuietWarning,
		AgentTimeout:            DefaultAgentTimeout,
		ReviewAgentTimeout:      DefaultReviewAgentTimeout,
		TestAgentTimeout:        DefaultTestAgentTimeout,
		DaemonConnectTimeout:    DefaultDaemonConnectTimeout,
		BranchSyncRemoteTimeout: DefaultBranchSyncRemoteTimeout,
		GateReconcileInterval:   DefaultGateReconcileInterval,
		GateReconcileTimeout:    DefaultGateReconcileTimeout,
		LogLevel:                "info",
		SessionReuse:            true,
		Eval:                    evalDefaults(),
	}
}

// GlobalConfigMappingEntry is one entry of a top-level mapping, spelled the way
// the document spells it.
type GlobalConfigMappingEntry struct {
	Key   string
	Value string
}

// GlobalConfigMapping describes how a top-level mapping key is written in the
// global config document. It is what decides which edit an operator can be told
// to make, and every field answers a question the parsed configuration cannot.
//
// Presence: `key:` with nothing after it and `key: {}` both decode to a map of
// length zero, exactly like an absent key, so anything that must not duplicate a
// top-level key has to ask the document. YAML rejects a duplicate top-level key
// outright, leaving a configuration that no longer loads.
//
// Shape: an entry line can be added under a key only when its value is a BLOCK
// mapping. After `key: {}` or `key: {a: b}` an indented entry line is not a
// continuation of the mapping at all - YAML rejects the document with "did not
// find expected key" - and after a valueless `key:` the safe edit is the same
// replacement, so both are reported as not appendable.
//
// Indentation: siblings of a block mapping all sit at the same column, so an
// entry line added at a different one is rejected the same way. The document is
// hand-maintained, so its indentation is whatever its operator chose.
type GlobalConfigMapping struct {
	// Present reports that the key is in the document, whatever its value.
	Present bool

	// AppendableBlock reports that one more indented entry line under the key is
	// a valid edit.
	AppendableBlock bool

	// EntryIndent is the column the key's entries start at, counted from zero, so
	// an added or replaced entry line matches its siblings. Zero when the key has
	// no entries to match.
	EntryIndent int

	// Line is the document line that spells the key, as written, so guidance can
	// name the line to replace. Empty when the key's value spans further lines.
	Line string

	// Entries are the key's entries in document order, so a replacement can
	// carry the ones the operator already has.
	Entries []GlobalConfigMappingEntry
}

// InspectGlobalConfigMapping describes the top-level key in the global config
// document at path.
//
// It never fails: a missing or unreadable file has no key, and a file this
// package cannot parse is scanned for the key written at the start of a line,
// which is where a top-level key is - reported as present but not appendable, so
// guidance falls back to naming the whole replacement. Callers that must not
// write a second top-level key are the reason presence is still answered for a
// document nothing could parse; every caller in this repository refuses such a
// configuration before it asks.
func InspectGlobalConfigMapping(path, key string) GlobalConfigMapping {
	data, err := os.ReadFile(path)
	if err != nil {
		return GlobalConfigMapping{}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return GlobalConfigMapping{Present: hasTopLevelKeyLine(data, key)}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return GlobalConfigMapping{}
	}
	mapping := doc.Content[0]
	if mapping.Kind != yaml.MappingNode {
		return GlobalConfigMapping{}
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		keyNode, value := mapping.Content[i], mapping.Content[i+1]
		if keyNode.Value != key {
			continue
		}
		found := GlobalConfigMapping{Present: true}
		if value.Kind == yaml.MappingNode {
			found.AppendableBlock = value.Style&yaml.FlowStyle == 0
			for j := 0; j+1 < len(value.Content); j += 2 {
				found.Entries = append(found.Entries, GlobalConfigMappingEntry{
					Key:   value.Content[j].Value,
					Value: value.Content[j+1].Value,
				})
			}
			if found.AppendableBlock && len(value.Content) > 0 && value.Content[0].Column > 1 {
				found.EntryIndent = value.Content[0].Column - 1
			}
		}
		if !found.AppendableBlock {
			found.Line = documentLine(data, keyNode, value)
		}
		return found
	}
	return GlobalConfigMapping{}
}

// documentLine returns the single line that holds the key and its whole value,
// empty when the value continues past it and no one line can be named.
func documentLine(data []byte, keyNode, value *yaml.Node) string {
	if lastNodeLine(value) != keyNode.Line {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if keyNode.Line < 1 || keyNode.Line > len(lines) {
		return ""
	}
	return strings.TrimRight(lines[keyNode.Line-1], " \t\r")
}

func lastNodeLine(n *yaml.Node) int {
	last := n.Line
	for _, child := range n.Content {
		if line := lastNodeLine(child); line > last {
			last = line
		}
	}
	return last
}

// hasTopLevelKeyLine is the fallback for a document YAML cannot parse: a
// top-level key starts its line, so an indented or commented occurrence is not
// one.
func hasTopLevelKeyLine(data []byte, key string) bool {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, key+":") {
			return true
		}
	}
	return false
}

// LoadGlobal reads global config from path. Returns defaults if file doesn't exist.
func LoadGlobal(path string) (*GlobalConfig, error) {
	cfg := DefaultGlobalConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			cfg.SourceYAML = []byte("{}\n")
			return cfg, nil
		}
		return nil, fmt.Errorf("read global config: %w", err)
	}
	return LoadGlobalFromBytes(data)
}

func LoadGlobalFromBytes(data []byte) (*GlobalConfig, error) {
	cfg := DefaultGlobalConfig()
	cfg.SourceYAML = append([]byte(nil), data...)
	var raw globalConfigRaw
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse global config: %w", err)
	}
	if err := validateCommitRaw(raw.Commit); err != nil {
		return nil, fmt.Errorf("parse global config: %w", err)
	}
	if err := validateTestRaw(raw.Test); err != nil {
		return nil, fmt.Errorf("parse global config: %w", err)
	}
	if err := validateEvalRaw(raw.Eval); err != nil {
		return nil, fmt.Errorf("parse global config: %w", err)
	}

	if len(raw.Agent) > 0 {
		cfg.Agents = copyAgents(raw.Agent)
		cfg.Agent = firstAgent(cfg.Agents)
	}
	if raw.ACPXPath != "" {
		cfg.ACPXPath = raw.ACPXPath
	}
	if raw.ForgejoAXIPath != "" {
		cfg.ForgejoAXIPath = raw.ForgejoAXIPath
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
	if raw.AgentConfig != nil {
		profiles, err := parseAgentConfig(raw.AgentConfig)
		if err != nil {
			return nil, err
		}
		cfg.AgentConfig = profiles
	}
	if raw.WorktreeRoots != nil {
		if err := ValidateWorktreeRoots(raw.WorktreeRoots); err != nil {
			return nil, err
		}
		cfg.WorktreeRoots = raw.WorktreeRoots
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
	if raw.AgentTimeout != "" {
		d, err := parsePositiveDuration("agent_timeout", raw.AgentTimeout)
		if err != nil {
			return nil, err
		}
		cfg.AgentTimeout = d
	}
	if raw.ReviewAgentTimeout != "" {
		d, err := parsePositiveDuration("review_agent_timeout", raw.ReviewAgentTimeout)
		if err != nil {
			return nil, err
		}
		cfg.ReviewAgentTimeout = d
	}
	if raw.TestAgentTimeout != "" {
		d, err := parsePositiveDuration("test_agent_timeout", raw.TestAgentTimeout)
		if err != nil {
			return nil, err
		}
		cfg.TestAgentTimeout = d
	}
	if raw.DaemonConnectTimeout != "" {
		d, err := parsePositiveDuration("daemon_connect_timeout", raw.DaemonConnectTimeout)
		if err != nil {
			return nil, err
		}
		cfg.DaemonConnectTimeout = d
	}
	if raw.BranchSyncRemoteTimeout != "" {
		d, err := parsePositiveDuration("branch_sync_remote_timeout", raw.BranchSyncRemoteTimeout)
		if err != nil {
			return nil, err
		}
		cfg.BranchSyncRemoteTimeout = d
	}
	if raw.GateReconcileInterval != "" {
		d, err := parsePositiveDuration("gate_reconcile_interval", raw.GateReconcileInterval)
		if err != nil {
			return nil, err
		}
		cfg.GateReconcileInterval = d
	}
	if raw.GateReconcileTimeout != "" {
		d, err := parsePositiveDuration("gate_reconcile_timeout", raw.GateReconcileTimeout)
		if err != nil {
			return nil, err
		}
		cfg.GateReconcileTimeout = d
	}
	if raw.LogLevel != "" {
		cfg.LogLevel = raw.LogLevel
	}
	if raw.SessionReuse != nil {
		cfg.SessionReuse = *raw.SessionReuse
	}
	if raw.ForgeProfiles != nil {
		profiles, err := normalizeForgeProfiles(raw.ForgeProfiles)
		if err != nil {
			return nil, err
		}
		cfg.ForgeProfiles = profiles
	}
	if raw.AutoFix.CI == nil {
		raw.AutoFix.CI = raw.AutoFix.Babysit
	}
	cfg.AutoFix = raw.AutoFix
	cfg.CI = raw.CI
	cfg.Commit = raw.Commit
	cfg.Intent = raw.Intent
	cfg.Test = raw.Test
	cfg.Providers = raw.Providers
	applyEvalOverrides(&cfg.Eval, &raw.Eval)

	return cfg, nil
}

func normalizeForgeProfiles(raw ForgeProfiles) (ForgeProfiles, error) {
	profiles := make(ForgeProfiles, len(raw))
	for host, profile := range raw {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" {
			return nil, fmt.Errorf("invalid forge_profiles: host must not be empty")
		}
		if _, exists := profiles[host]; exists {
			return nil, fmt.Errorf("invalid forge_profiles: duplicate host %q after case normalization", host)
		}
		ghDir := strings.TrimSpace(profile.GHConfigDir)
		glabDir := strings.TrimSpace(profile.GLabConfigDir)
		if (ghDir == "") == (glabDir == "") {
			return nil, fmt.Errorf("invalid forge_profiles.%s: exactly one of gh_config_dir or glab_config_dir is required", host)
		}
		profile.GHConfigDir = ghDir
		profile.GLabConfigDir = glabDir
		profile.ExpectedLogin = strings.TrimSpace(profile.ExpectedLogin)
		if ghDir != "" {
			normalized, err := normalizeForgeProfilePath(ghDir)
			if err != nil {
				return nil, fmt.Errorf("invalid forge_profiles.%s.gh_config_dir: %w", host, err)
			}
			profile.GHConfigDir = normalized
		} else {
			normalized, err := normalizeForgeProfilePath(glabDir)
			if err != nil {
				return nil, fmt.Errorf("invalid forge_profiles.%s.glab_config_dir: %w", host, err)
			}
			profile.GLabConfigDir = normalized
		}
		profiles[host] = profile
	}
	return profiles, nil
}

func normalizeForgeProfilePath(value string) (string, error) {
	if strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if home == "" {
			return "", fmt.Errorf("resolve home directory: empty path")
		}
		return filepath.Clean(filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(value, "~/")))), nil
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("path must be absolute or start with ~/")
	}
	return filepath.Clean(value), nil
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
	if err := validateCommitRaw(cfg.Commit); err != nil {
		return nil, fmt.Errorf("parse repo config: %w", err)
	}
	if err := validateReviewRaw(cfg.Review); err != nil {
		return nil, fmt.Errorf("parse repo config: %w", err)
	}
	for i, pattern := range cfg.ProtectedPaths {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			return nil, fmt.Errorf("protected_paths[%d] must not be empty", i)
		}
		if err := validatePathInstructionGlob(pattern); err != nil {
			return nil, fmt.Errorf("protected_paths[%d] %q is not a valid glob: %w", i, pattern, err)
		}
		cfg.ProtectedPaths[i] = pattern
	}
	if err := validateTestRaw(cfg.Test); err != nil {
		return nil, fmt.Errorf("parse repo config: %w", err)
	}
	cfg.PR.BaseBranch = strings.TrimSpace(cfg.PR.BaseBranch)
	if err := validatePRRaw(cfg.PR); err != nil {
		return nil, fmt.Errorf("parse repo config: %w", err)
	}
	if cfg.AutoFix.CI == nil {
		cfg.AutoFix.CI = cfg.AutoFix.Babysit
	}

	return cfg, nil
}

// validatePRRaw fails the config closed on a pr.base_branch value Git would
// reject as a branch name, the same convention validateTestRaw already
// applies to test.evidence.branch. An empty value is valid: it means "fall
// back to the repository's forge default branch" and is intentionally not
// normalized to any particular name here.
func validatePRRaw(pr PRRaw) error {
	if pr.BaseBranch == "" {
		return nil
	}
	if _, err := evidence.NormalizeBranch(pr.BaseBranch); err != nil {
		return fmt.Errorf("pr.base_branch: %w", err)
	}
	return nil
}

// validateReviewRaw fails the config closed on a review.path_instructions list
// the review step could not honor deterministically: a missing path or
// instructions value, a glob the matcher cannot compile, or a list that would
// overrun the review prompt budget. Rejecting the config aborts the run before
// an agent starts, which is preferable to silently dropping guidance the
// maintainer expects the reviewer to apply.
//
// This deliberately also runs on the PUSHED copy, even though EffectiveRepoConfig
// discards a pushed review block: the trusted-copy read
// (assertGateTrustedConfigReadable in internal/daemon) aborts EVERY run whose
// default-branch .no-mistakes.yaml fails these checks, so a branch carrying an
// invalid block has to fail here, before it merges, rather than brick the
// repository's pipeline afterwards. Do not scope this to the trusted copy.
func validateReviewRaw(review ReviewRaw) error {
	if len(review.PathInstructions) > MaxReviewPathInstructions {
		return fmt.Errorf("review.path_instructions has %d entries, at most %d are allowed", len(review.PathInstructions), MaxReviewPathInstructions)
	}
	for i, entry := range review.PathInstructions {
		path := strings.TrimSpace(entry.Path)
		if path == "" {
			return fmt.Errorf("review.path_instructions[%d].path must not be empty", i)
		}
		if strings.TrimSpace(entry.Instructions) == "" {
			return fmt.Errorf("review.path_instructions[%d].instructions must not be empty (path %q)", i, path)
		}
		if RenderedInstructions(entry.Instructions) == "" {
			return fmt.Errorf("review.path_instructions[%d].instructions for path %q is left empty once merge-conflict markers are removed; write the rule without <<<<<<<, =======, or >>>>>>>", i, path)
		}
		if err := validatePathInstructionGlob(path); err != nil {
			return fmt.Errorf("review.path_instructions[%d].path %q is not a valid glob: %w", i, path, err)
		}
	}
	if total := ReviewPathInstructionsBytes(review.PathInstructions); total > MaxReviewPathInstructionsBytes {
		return fmt.Errorf("review.path_instructions would add up to %d bytes to the review prompt, at most %d are allowed so the prompt stays within budget", total, MaxReviewPathInstructionsBytes)
	}
	return nil
}

// validatePathInstructionGlob mirrors how ignore_patterns are matched: a
// trailing "/**" is a literal subtree prefix rather than a glob, and everything
// else goes through path.Match, so only patterns Match can compile are accepted.
// It must stay path.Match rather than filepath.Match for the same reason the
// matcher does (matchIgnorePattern in internal/pipeline/steps): filepath.Match
// is separator-dependent, so on Windows the validator would accept patterns the
// matcher rejects and read a "\" as a path separator instead of an escape.
func validatePathInstructionGlob(pattern string) error {
	if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
		if prefix == "" {
			return errors.New("subtree pattern needs a directory before /**")
		}
		return nil
	}
	if _, err := path.Match(pattern, "a"); err != nil {
		return err
	}
	return nil
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
// documentation rules that gate itself. Review (the path-scoped guidance
// injected into the review gate prompt) is trusted-only for the same reason: a
// pushed branch must not steer the reviewer that gates it. DisableProjectSettings
// is also trusted-only so a pushed branch cannot enable or defeat the gate-agent
// project-instruction boundary. NoCI is trusted-only so a pushed branch cannot
// self-declare no-CI and bypass its own checks, and CI (the transient-rerun
// budget) is trusted-only because every rerun it authorizes is another
// provider-side workflow run billed to the repository. These gate-control
// fields ignore allowRepoCommands. PR is the explicit exception: the
// allowRepoCommands opt-in also permits a pushed PR target because it controls
// where a maintainer-authorized PR lands, not code execution.
// When allowRepoCommands is
// true the maintainer has explicitly opted in (via allow_repo_commands on the
// TRUSTED default-branch copy) to honoring the pushed branch's commands and
// agent selection.
// When there is no trusted copy and the maintainer has not opted in, both
// fields are forced empty (Agent "" and nil Agents inherit the global agent;
// Commands{} yields built-in defaults) rather than falling back to the pushed
// branch - this blocks the supply-chain vector for repos that ship
// .no-mistakes.yaml only on feature branches.
//
// Non-executing fields (ignore patterns, auto-fix, commit, intent, test, and
// providers) are always taken from the pushed copy, matching prior behavior,
// since they cannot run arbitrary shell, select a process, or spend the
// maintainer's CI minutes.
// The single exception inside test is evidence.branch, which names a git ref
// the daemon pushes to and is therefore trusted-only.
func EffectiveRepoConfig(pushed, trusted *RepoConfig, allowRepoCommands bool) *RepoConfig {
	if pushed == nil {
		pushed = &RepoConfig{}
	}
	effective := *pushed
	if trusted != nil {
		effective.Document = trusted.Document
		effective.ProtectedPaths = append([]string(nil), trusted.ProtectedPaths...)
		// review.path_instructions steers the gate agent that reviews the pushed
		// branch, so it is trusted-only exactly like document.instructions and
		// regardless of allow_repo_commands: a contributor must not be able to
		// inject rules into their own review, and enabling the commands opt-in
		// must not silently drop the maintainer's review rules when the pushed
		// branch happens to carry no review block.
		effective.Review = trusted.Review
		// disable_project_settings is a security boundary: honor it ONLY from the
		// trusted default-branch copy so a pushed branch cannot turn the opt-out
		// off (and re-enable its own AGENTS.md) or on. A nil trusted copy here
		// means the trusted config was legitimately absent (the daemon aborts
		// separately when it could not be READ at all), so falsy is correct.
		effective.DisableProjectSettings = trusted.DisableProjectSettings
		// no_ci is a readiness boundary: honor it ONLY from the trusted
		// default-branch copy so a pushed branch cannot self-declare no-CI and
		// bypass checks that the default branch still expects.
		effective.NoCI = trusted.NoCI
		// The whole ci block is trusted-only. ci.rerun_transient spends the
		// maintainer's resources rather than the contributor's: every rerun is
		// another provider-side workflow run billed to the repository, so a
		// pushed branch must not be able to raise its own rerun budget to the
		// cap. ci.revalidate_repairs is a validation boundary in the same
		// sense: it decides whether a CI repair commit must re-pass Review
		// before it is published, so a pushed branch must not be able to turn
		// the maintainer's revalidation requirement off for its own repairs.
		effective.CI = trusted.CI
		// test.evidence.branch names the git ref evidence commits are pushed
		// to with the maintainer's credentials. It is trusted-only so a pushed
		// branch cannot aim them at another branch of the repository; the rest
		// of test.evidence (store_in_repo, attach_media, dir) stays
		// pushed-readable because it only picks how artifacts are published.
		// The publisher independently refuses any branch without its marker
		// file, and the upload client refuses Actions/App installation tokens,
		// so this is defense in depth.
		effective.Test.Evidence.Branch = trusted.Test.Evidence.Branch
		// pr.base_branch controls where the contributor's PR lands, so it is
		// trusted-only unless the repository explicitly opts into pushed
		// settings alongside commands and agent selection.
		if !allowRepoCommands {
			effective.PR = trusted.PR
		}
	} else {
		effective.Document = DocumentRaw{}
		effective.ProtectedPaths = nil
		effective.Review = ReviewRaw{}
		effective.DisableProjectSettings = false
		effective.NoCI = false
		effective.CI = CIRaw{}
		effective.Test.Evidence.Branch = nil
		if !allowRepoCommands {
			effective.PR = PRRaw{}
		}
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

// testDefaults returns the default test-step settings. Orphan-branch evidence
// publication is opt-in (off by default). GitHub image/video attachments at PR
// render time are on by default so remote reviewers can open screenshots
// without that branch.
func testDefaults() Test {
	return Test{
		Evidence: Evidence{
			StoreInRepo: false,
			AttachMedia: true,
			Dir:         ".no-mistakes/evidence",
			Branch:      evidence.DefaultBranch,
			LocalRoot:   "",
			Retention:   DefaultEvidenceRetention,
			MaxRuns:     DefaultEvidenceMaxRuns,
		},
	}
}

// applyTestOverrides applies non-nil raw values onto resolved defaults.
// The branch name is validated at config parse time (validateTestRaw), so an
// unusable value never reaches here.
//
// It deliberately covers only the repository-relevant half of test.evidence.
// The local-storage half is applied separately by applyEvidenceStorageOverrides
// so a repository config can never reach it (see EvidenceRaw.LocalRoot).
func applyTestOverrides(dst *Test, src *TestRaw) {
	if src.Evidence.StoreInRepo != nil {
		dst.Evidence.StoreInRepo = *src.Evidence.StoreInRepo
	}
	if src.Evidence.AttachMedia != nil {
		dst.Evidence.AttachMedia = *src.Evidence.AttachMedia
	}
	if src.Evidence.Dir != nil && strings.TrimSpace(*src.Evidence.Dir) != "" {
		dst.Evidence.Dir = strings.TrimSpace(*src.Evidence.Dir)
	}
	if src.Evidence.Branch != nil && strings.TrimSpace(*src.Evidence.Branch) != "" {
		if branch, err := evidence.NormalizeBranch(*src.Evidence.Branch); err == nil {
			dst.Evidence.Branch = branch
		}
	}
}

// applyEvidenceStorageOverrides applies the global-only local-storage half of
// test.evidence. Merge calls it with the GlobalConfig copy and nothing else, so
// neither a pushed nor a trusted repository config can move the daemon's
// evidence directory or change its retention budget. Values are validated at
// config parse time (validateTestRaw), so an unusable value never reaches here.
func applyEvidenceStorageOverrides(dst *Evidence, src *EvidenceRaw) {
	if src.LocalRoot != nil && strings.TrimSpace(*src.LocalRoot) != "" {
		dst.LocalRoot = strings.TrimSpace(*src.LocalRoot)
	}
	if src.Retention != nil {
		if d, err := parseEvidenceRetention(*src.Retention); err == nil {
			dst.Retention = d
		}
	}
	if src.MaxRuns != nil && *src.MaxRuns >= 0 {
		dst.MaxRuns = *src.MaxRuns
	}
}

// parseEvidenceRetention interprets test.evidence.retention. The keyword
// "unlimited" (also "none"/"off"/"never"), or any non-positive duration,
// disables age-based reaping and resolves to 0, which keeps every run's
// evidence until the max_runs ceiling removes it.
func parseEvidenceRetention(value string) (time.Duration, error) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	switch trimmed {
	case "":
		return DefaultEvidenceRetention, nil
	case "unlimited", "none", "off", "never":
		return 0, nil
	}
	d, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("test.evidence.retention: parse %q: %w", value, err)
	}
	if d <= 0 {
		return 0, nil
	}
	return d, nil
}

// evalDefaults returns the default local evaluation-corpus settings. Both
// halves are on by default: provenance is unrecoverable if it was not recorded
// at review time, and a corpus nobody has to remember to collect is the only
// kind that exists when a comparison is finally needed. The default cap keeps
// the corpus a rolling window rather than an unbounded archive.
func evalDefaults() Eval {
	return Eval{CaptureProvenance: true, AutoCapture: true, MaxCases: DefaultEvalMaxCases, DiversifiedSize: DefaultEvalDiversifiedSize}
}

// applyEvalOverrides applies non-nil raw values onto resolved defaults. The
// max_cases value is validated at config parse time (validateEvalRaw).
func applyEvalOverrides(dst *Eval, src *EvalRaw) {
	if src.CaptureProvenance != nil {
		dst.CaptureProvenance = *src.CaptureProvenance
	}
	if src.AutoCapture != nil {
		dst.AutoCapture = *src.AutoCapture
	}
	if src.MaxCases != nil && *src.MaxCases >= 0 {
		dst.MaxCases = *src.MaxCases
	}
	if src.DiversifiedSize != nil && *src.DiversifiedSize >= 0 {
		dst.DiversifiedSize = *src.DiversifiedSize
	}
}

// validateEvalRaw fails the config closed on a negative eval.max_cases. A
// negative cap has no defensible meaning here - it is neither "keep everything"
// (0) nor a bound - so surfacing the typo beats guessing which one was meant.
func validateEvalRaw(raw EvalRaw) error {
	if raw.MaxCases != nil && *raw.MaxCases < 0 {
		return fmt.Errorf("eval.max_cases must be 0 (keep every case) or greater, got %d", *raw.MaxCases)
	}
	if raw.DiversifiedSize != nil && *raw.DiversifiedSize < 0 {
		return fmt.Errorf("eval.diversified_size must be 0 (one gold case per stratum) or greater, got %d", *raw.DiversifiedSize)
	}
	return nil
}

// validateTestRaw fails the config closed on a test.evidence.branch value Git
// would reject as a branch name. Rejecting the config surfaces the typo where
// the user can fix it, rather than letting a run reach the push and fail there.
//
// Like validateReviewRaw this deliberately also runs on the PUSHED copy even
// though EffectiveRepoConfig only honors the trusted branch name: a branch
// carrying an invalid value has to fail before it merges.
func validateTestRaw(test TestRaw) error {
	if test.Evidence.Branch != nil {
		if _, err := evidence.NormalizeBranch(*test.Evidence.Branch); err != nil {
			return fmt.Errorf("test.evidence.branch: %w", err)
		}
	}
	// local_root must be absolute. The daemon's working directory is a bare
	// gate repository, so a relative path would resolve somewhere the operator
	// never named - and evidence would silently scatter instead of landing
	// where they asked. Surface the mistake in the config rather than at run
	// time. Like branch, this also validates the PUSHED copy even though the
	// value is honored only from the global config: a branch carrying an
	// invalid value has to fail before it merges.
	if test.Evidence.LocalRoot != nil {
		root := strings.TrimSpace(*test.Evidence.LocalRoot)
		if root != "" && !filepath.IsAbs(root) {
			return fmt.Errorf("test.evidence.local_root must be an absolute path, got %q", root)
		}
	}
	if test.Evidence.Retention != nil {
		if _, err := parseEvidenceRetention(*test.Evidence.Retention); err != nil {
			return err
		}
	}
	if test.Evidence.MaxRuns != nil && *test.Evidence.MaxRuns < 0 {
		return fmt.Errorf("test.evidence.max_runs must be 0 (keep every run) or greater, got %d", *test.Evidence.MaxRuns)
	}
	return nil
}

// applyProvidersOverrides applies non-nil raw values onto resolved defaults.
func applyProvidersOverrides(dst *Providers, src *ProvidersRaw) {
	if src.GitHub.DraftPullRequests != nil {
		dst.GitHub.DraftPullRequests = *src.GitHub.DraftPullRequests
	}
	if src.GitLab.DraftPullRequests != nil {
		dst.GitLab.DraftPullRequests = *src.GitLab.DraftPullRequests
	}
	if src.Bitbucket.DraftPullRequests != nil {
		dst.Bitbucket.DraftPullRequests = *src.Bitbucket.DraftPullRequests
	}
	if src.AzureDevOps.DraftPullRequests != nil {
		dst.AzureDevOps.DraftPullRequests = *src.AzureDevOps.DraftPullRequests
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

// ciDefaults returns the default CI-step settings. Rerunning cancelled checks
// is off by default: a CANCELLED conclusion does not say who cancelled, so the
// safe baseline is to escalate rather than risk restarting a job a maintainer
// or a concurrency rule deliberately stopped. Repositories that know their
// cancellations are provider-side opt in via ci.rerun_transient.
// Post-repair revalidation is off for the reason recorded on
// DefaultCIRevalidateRepairs: it is the pipeline's most expensive single
// behavior, so it is opted into rather than paid for by default.
func ciDefaults() CI {
	return CI{
		RerunTransient:    DefaultCIRerunTransient,
		RevalidateRepairs: DefaultCIRevalidateRepairs,
	}
}

// applyCIOverrides applies non-nil raw values onto resolved defaults, clamping
// the rerun budget into range: a negative value disables reruns rather than
// inverting the bound, and anything above MaxCIRerunTransient is capped so a
// typo cannot keep a run polling one commit indefinitely.
func applyCIOverrides(dst *CI, src *CIRaw) {
	if src.RerunTransient != nil {
		dst.RerunTransient = min(max(*src.RerunTransient, 0), MaxCIRerunTransient)
	}
	// Applied independently of the rerun budget so a config that sets only one
	// of the two keys does not silently discard the other, and so an explicit
	// `revalidate_repairs: false` in the later (repository) source overrides an
	// earlier `true` rather than reading as "unset".
	if src.RevalidateRepairs != nil {
		dst.RevalidateRepairs = *src.RevalidateRepairs
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

	ci := ciDefaults()
	// The operator's global value is a machine-wide floor they can always set;
	// the repo value is trusted-only (EffectiveRepoConfig sourced it from the
	// default branch), so the maintainer of the repository still has the last
	// word on how many workflow runs their project is billed for.
	applyCIOverrides(&ci, &global.CI)
	applyCIOverrides(&ci, &repo.CI)

	intent := intentDefaults()
	applyIntentOverrides(&intent, &global.Intent)
	applyIntentOverrides(&intent, &repo.Intent)

	test := testDefaults()
	applyTestOverrides(&test, &global.Test)
	applyTestOverrides(&test, &repo.Test)
	// Applied last and from the global config only: where the daemon writes
	// evidence on this machine, and how long it keeps it, is never a
	// repository's decision (see EvidenceRaw.LocalRoot).
	applyEvidenceStorageOverrides(&test.Evidence, &global.Test.Evidence)

	commit := Commit{FixMessage: DefaultFixMessageTemplate}
	if global.Commit.FixMessage != nil {
		commit.FixMessage = *global.Commit.FixMessage
	}
	if repo.Commit.FixMessage != nil {
		commit.FixMessage = *repo.Commit.FixMessage
	}

	providers := Providers{}
	applyProvidersOverrides(&providers, &global.Providers)
	applyProvidersOverrides(&providers, &repo.Providers)

	cfg := &Config{
		Agent:                 global.Agent,
		Agents:                copyAgents(global.Agents),
		ACPXPath:              global.ACPXPath,
		ForgejoAXIPath:        global.ForgejoAXIPath,
		ACPRegistryOverrides:  global.ACPRegistryOverrides,
		AgentPathOverride:     global.AgentPathOverride,
		AgentArgsOverride:     global.AgentArgsOverride,
		AgentConfig:           global.AgentConfig,
		CITimeout:             global.CITimeout,
		StepQuietWarning:      global.StepQuietWarning,
		AgentTimeout:          global.AgentTimeout,
		ReviewAgentTimeout:    global.ReviewAgentTimeout,
		TestAgentTimeout:      global.TestAgentTimeout,
		GateReconcileInterval: global.GateReconcileInterval,
		GateReconcileTimeout:  global.GateReconcileTimeout,
		LogLevel:              global.LogLevel,
		SessionReuse:          global.SessionReuse,
		// Eval is global-only by design (see GlobalConfig.Eval), so it is
		// copied straight through with no repository override step.
		Eval:           global.Eval,
		Commands:       repo.Commands,
		IgnorePatterns: repo.IgnorePatterns,
		ProtectedPaths: repo.ProtectedPaths,
		AutoFix:        af,
		CI:             ci,
		Commit:         commit,
		Intent:         intent,
		Test:           test,
		Document:       Document{Instructions: strings.TrimSpace(repo.Document.Instructions)},
		Review:         Review{PathInstructions: resolvePathInstructions(repo.Review.PathInstructions)},
		PR:             PR{BaseBranch: strings.TrimSpace(repo.PR.BaseBranch)},
		ForgeProfiles:  global.ForgeProfiles,
		Providers:      providers,
		// repo is the EffectiveRepoConfig result, so this value is already
		// trusted-only (EffectiveRepoConfig sourced it from the trusted copy).
		DisableProjectSettings: repo.DisableProjectSettings,
		NoCI:                   repo.NoCI,
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

// EnableEvalProvenance pins the exact configuration this run reviews under so
// a later replay grades a candidate against identical conditions. The caller
// decides whether to call it (see Eval.CaptureProvenance); this is the single
// owner of what "exact provenance" contains.
func (c *Config) EnableEvalProvenance(global *GlobalConfig, repo *RepoConfig) error {
	if c == nil || global == nil || repo == nil {
		return fmt.Errorf("eval provenance requires merged, global, and repository configuration")
	}
	repoYAML, err := yaml.Marshal(repo)
	if err != nil {
		return fmt.Errorf("serialize eval repository configuration: %w", err)
	}
	c.ReplayGlobalYAML = append([]byte(nil), global.SourceYAML...)
	c.ReplayRepoYAML = repoYAML
	c.CaptureEvalProvenance = true
	return nil
}
