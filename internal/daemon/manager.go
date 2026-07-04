package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// StepFactory creates pipeline steps for a run from its merged config.
// Defaults to steps.BuildPipeline over cfg.Steps (the repo's optional
// `steps:` selection; empty means the full default pipeline). An error fails
// the run at start — a bad steps config must never silently fall back.
type StepFactory func(cfg *config.Config) ([]pipeline.Step, error)

// RunManager tracks active pipeline executors and manages run lifecycle.
type RunManager struct {
	mu           sync.Mutex
	executors    map[string]*pipeline.Executor      // runID → executor
	cancels      map[string]context.CancelCauseFunc // runID → cancel function with cause
	dones        map[string]chan struct{}           // runID → closed when goroutine exits
	wg           sync.WaitGroup                     // tracks background run goroutines
	shuttingDown atomic.Bool                        // prevents new runs during shutdown
	db           *db.DB
	paths        *paths.Paths
	steps        StepFactory

	branchLocks sync.Map // repoID+"/"+branch → *sync.Mutex

	subMu          sync.RWMutex
	subscribers    map[string][]chan<- ipc.Event // runID → subscriber channels
	completedRuns  map[string]bool               // runIDs whose goroutines have finished
	completedOrder []string                      // insertion order for FIFO eviction
}

// NewRunManager creates a RunManager. Pass nil for stepFactory to use default steps.
func NewRunManager(database *db.DB, p *paths.Paths, stepFactory StepFactory) *RunManager {
	if stepFactory == nil {
		stepFactory = func(cfg *config.Config) ([]pipeline.Step, error) {
			var stepSpecs []config.StepSpec
			if cfg != nil {
				stepSpecs = cfg.Steps
			}
			return steps.BuildPipeline(stepSpecs)
		}
	}
	return &RunManager{
		executors:     make(map[string]*pipeline.Executor),
		cancels:       make(map[string]context.CancelCauseFunc),
		dones:         make(map[string]chan struct{}),
		db:            database,
		paths:         p,
		steps:         stepFactory,
		subscribers:   make(map[string][]chan<- ipc.Event),
		completedRuns: make(map[string]bool),
	}
}

// Subscribe registers a channel to receive events for a run.
// Returns the channel and an unsubscribe function.
// If the run has already completed, the returned channel is immediately closed.
func (m *RunManager) Subscribe(runID string) (<-chan ipc.Event, func()) {
	ch := make(chan ipc.Event, 64)
	m.subMu.Lock()
	if m.completedRuns[runID] {
		m.subMu.Unlock()
		close(ch)
		return ch, func() {}
	}
	m.subscribers[runID] = append(m.subscribers[runID], ch)
	m.subMu.Unlock()

	unsub := func() {
		m.subMu.Lock()
		defer m.subMu.Unlock()
		subs := m.subscribers[runID]
		for i, s := range subs {
			if s == ch {
				m.subscribers[runID] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
	}
	return ch, unsub
}

// broadcast sends an event to all subscribers of the event's run.
func (m *RunManager) broadcast(event ipc.Event) {
	m.subMu.RLock()
	defer m.subMu.RUnlock()
	for _, ch := range m.subscribers[event.RunID] {
		select {
		case ch <- event:
		default:
			slog.Debug("dropped event for slow subscriber", "run_id", event.RunID, "type", event.Type)
		}
	}
}

// closeSubscribers closes all subscriber channels for a run and marks it
// as completed so future Subscribe calls return an immediately-closed channel.
func (m *RunManager) closeSubscribers(runID string) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	for _, ch := range m.subscribers[runID] {
		close(ch)
	}
	delete(m.subscribers, runID)
	m.completedRuns[runID] = true
	m.completedOrder = append(m.completedOrder, runID)
	if len(m.completedOrder) > 1000 {
		half := len(m.completedOrder) / 2
		for _, id := range m.completedOrder[:half] {
			delete(m.completedRuns, id)
		}
		m.completedOrder = m.completedOrder[half:]
	}
}

// repoIDFromGatePath extracts the repo ID from a gate bare repo path.
// Gate paths look like: <root>/repos/<id>.git
func repoIDFromGatePath(gatePath string) (string, error) {
	base := filepath.Base(gatePath)
	if !strings.HasSuffix(base, ".git") {
		return "", fmt.Errorf("invalid gate path: %s", gatePath)
	}
	return strings.TrimSuffix(base, ".git"), nil
}

// branchFromRef extracts the branch name from a full git ref.
// "refs/heads/main" → "main", "main" → "main"
func branchFromRef(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

// loadTrustedRepoConfig reads .no-mistakes.yaml from the trusted
// default-branch commit (trustedSHA — the exact SHA startRun just fetched and
// resolved) in the worktree and parses it. Reading at a pinned SHA, rather
// than the origin/<defaultBranch> remote-tracking ref, closes the stale-ref
// hole: the gate worktree shares refs with the bare repo, so without a fresh
// fetch + resolve the ref could point at a commit a previous run left behind.
//
// trustedSHA is empty when the default branch is unknown, the fetch failed,
// or the ref did not resolve — every one of those failure modes returns nil
// here so the caller (EffectiveRepoConfig) fails closed: the pushed branch's
// commands and agent are dropped and the run proceeds on built-in defaults.
// None of these are fatal, since the pushed-branch copy is still read for
// non-executing fields.
func loadTrustedRepoConfig(ctx context.Context, wtDir, trustedSHA, runID string) *config.RepoConfig {
	if trustedSHA == "" {
		// No trusted SHA means no freshly-fetched default-branch commit to
		// read from. Return nil so EffectiveRepoConfig forces empty
		// commands/agent — the secure default — instead of falling back to a
		// potentially stale origin/<defaultBranch> ref.
		return nil
	}
	content, err := git.ShowFile(ctx, wtDir, trustedSHA, ".no-mistakes.yaml")
	if err != nil {
		// Path absent on the default branch is the common "repo has no
		// trusted commands" case; log at debug so it isn't noisy. Other
		// errors are surfaced at warn so a genuinely broken read isn't
		// silent. Either way trusted is nil → fail closed.
		slog.Debug("trusted repo config: not present on default branch", "run_id", runID, "sha", trustedSHA, "error", err)
		return nil
	}
	trusted, err := config.LoadRepoFromBytes([]byte(content))
	if err != nil {
		slog.Warn("trusted repo config: parse failed; commands/agent from pushed branch will be disabled", "run_id", runID, "sha", trustedSHA, "error", err)
		return nil
	}
	return trusted
}

// loadTrustedStepInstructions reads every instruction file declared on the
// run's steps at the trusted default-branch SHA and returns their concatenated
// contents (each section prefixed with its path).
//
// SECURITY: instruction content is read at trustedSHA via `git show`, never
// from the pushed worktree, so a contributor's branch cannot rewrite the
// guidance injected into the gate's own agent steps. This holds even under
// allow_repo_commands: the *paths* may then come from the pushed branch, but
// the *content* is always the trusted default-branch copy. With no trusted SHA
// (fetch failed, no default branch) it fails closed — no instructions. A file
// absent at trustedSHA (or an unreadable one) simply contributes nothing.
func loadTrustedStepInstructions(ctx context.Context, wtDir, trustedSHA string, specs []config.StepSpec, runID string) string {
	if trustedSHA == "" {
		return ""
	}
	seen := make(map[string]bool)
	var sections []string
	for _, spec := range specs {
		for _, path := range spec.Instructions {
			path = strings.TrimSpace(path)
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			content, err := git.ShowFile(ctx, wtDir, trustedSHA, path)
			if err != nil {
				slog.Warn("step instructions: file not present on trusted default branch; skipping", "run_id", runID, "sha", trustedSHA, "path", path, "error", err)
				continue
			}
			if content = strings.TrimSpace(content); content == "" {
				continue
			}
			sections = append(sections, "# "+path+"\n"+content)
		}
	}
	return strings.Join(sections, "\n\n")
}

// loadTrustedSkillBodies resolves the body of every skill-driven step at the
// trusted default-branch SHA and returns a copy of specs with StepSpec.SkillBody
// populated. Non-skill specs pass through unchanged.
//
// SECURITY: like commands, agent, and step instructions, a skill body steers
// the maintainer's agent, so its content is read at trustedSHA via `git show`,
// never from the pushed worktree. A contributor's branch can therefore never
// rewrite the prompt that drives the review — even when allow_repo_commands
// lets the skill *path* come from the pushed branch, the *content* is always
// the trusted default-branch copy (the `steps:` list itself is already a
// trusted-only field under the secure default, so the path is trusted too).
// With no trusted SHA (fetch failed, no default branch) it fails closed: the
// body stays empty and the skill step parks with a misconfiguration finding
// rather than running an empty or pushed-branch prompt. A skill file absent at
// trustedSHA (or unreadable) likewise yields an empty body.
func loadTrustedSkillBodies(ctx context.Context, wtDir, trustedSHA string, specs []config.StepSpec, runID string) []config.StepSpec {
	hasSkill := false
	for _, spec := range specs {
		if spec.IsSkill() {
			hasSkill = true
			break
		}
	}
	if !hasSkill {
		return specs
	}
	resolved := make([]config.StepSpec, len(specs))
	copy(resolved, specs)
	for i := range resolved {
		if !resolved[i].IsSkill() {
			continue
		}
		if trustedSHA == "" {
			resolved[i].SkillBody = ""
			continue
		}
		content, err := git.ShowFile(ctx, wtDir, trustedSHA, resolved[i].Skill)
		if err != nil {
			slog.Warn("skill step: file not present on trusted default branch; step will park", "run_id", runID, "sha", trustedSHA, "path", resolved[i].Skill, "error", err)
			resolved[i].SkillBody = ""
			continue
		}
		resolved[i].SkillBody = content
	}
	return resolved
}

// loadProfile resolves the shared gate profile named by a repo's trusted
// `profile:` field to its parsed profile.yaml plus the on-disk profile
// directory. Both are host-local under <NM_HOME>/profiles/<name>/, a path no
// pushed commit can address, so nothing here is read from the worktree.
//
// It fails closed (returns an error) when the name is unsafe, the directory or
// profile.yaml is missing/unreadable, or profile.yaml does not parse — a
// missing/broken profile is a run-start error, never a silent fall back to the
// default pipeline (mirrors a bad steps: config).
func (m *RunManager) loadProfile(name string) (*config.ProfileConfig, string, error) {
	if !steps.ValidCustomName(name) {
		return nil, "", fmt.Errorf("invalid profile name %q (use lowercase letters, digits, '-' and '_', starting with a letter or digit)", name)
	}
	profileDir := m.paths.ProfileDir(name)
	profilePath := m.paths.ProfileFile(name)
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return nil, "", fmt.Errorf("read profile.yaml at %s: %w", profilePath, err)
	}
	profile, err := config.LoadProfileFromBytes(data)
	if err != nil {
		return nil, "", fmt.Errorf("parse profile %q: %w", name, err)
	}
	return profile, profileDir, nil
}

// profilePathWithinDir joins a profile-relative file path onto the profile
// directory and confirms the result does not escape it (defense in depth: the
// merged step list is also validated by validateStepSpecs, which rejects
// absolute paths and ".."). It returns the cleaned absolute path and whether it
// is safe to read.
func profilePathWithinDir(profileDir, rel string) (string, bool) {
	if strings.TrimSpace(rel) == "" || filepath.IsAbs(rel) {
		return "", false
	}
	full := filepath.Join(profileDir, rel)
	relCheck, err := filepath.Rel(profileDir, full)
	if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", false
	}
	return full, true
}

// loadProfileSkillBodies resolves each skill-driven profile step's body from a
// file under the profile directory (host-local disk), returning a copy of specs
// with StepSpec.SkillBody populated. Non-skill specs pass through unchanged.
//
// Unlike loadTrustedSkillBodies (which reads repo skills at the trusted
// default-branch SHA via git show), profile skills live under <NM_HOME>/
// profiles and are read straight from disk. A path that escapes the profile
// directory, or a missing/unreadable file, yields an empty body → the skill
// step parks with a misconfiguration finding rather than running an empty
// prompt.
func loadProfileSkillBodies(profileDir string, specs []config.StepSpec, runID string) []config.StepSpec {
	resolved := make([]config.StepSpec, len(specs))
	copy(resolved, specs)
	for i := range resolved {
		if !resolved[i].IsSkill() {
			continue
		}
		full, ok := profilePathWithinDir(profileDir, resolved[i].Skill)
		if !ok {
			slog.Warn("profile skill step: path escapes profile dir; step will park", "run_id", runID, "profile_dir", profileDir, "path", resolved[i].Skill)
			resolved[i].SkillBody = ""
			continue
		}
		content, err := os.ReadFile(full)
		if err != nil {
			slog.Warn("profile skill step: file not present in profile dir; step will park", "run_id", runID, "profile_dir", profileDir, "path", resolved[i].Skill, "error", err)
			resolved[i].SkillBody = ""
			continue
		}
		resolved[i].SkillBody = string(content)
	}
	return resolved
}

// loadProfileStepInstructions reads every instruction file declared on a
// profile's steps from the profile directory (host-local disk) and returns
// their concatenated contents, mirroring loadTrustedStepInstructions but
// resolving paths against the profile dir rather than the trusted worktree SHA.
// A path that escapes the profile dir, or a missing/unreadable file,
// contributes nothing.
func loadProfileStepInstructions(profileDir string, specs []config.StepSpec, runID string) string {
	seen := make(map[string]bool)
	var sections []string
	for _, spec := range specs {
		for _, path := range spec.Instructions {
			path = strings.TrimSpace(path)
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			full, ok := profilePathWithinDir(profileDir, path)
			if !ok {
				slog.Warn("profile step instructions: path escapes profile dir; skipping", "run_id", runID, "profile_dir", profileDir, "path", path)
				continue
			}
			content, err := os.ReadFile(full)
			if err != nil {
				slog.Warn("profile step instructions: file not present in profile dir; skipping", "run_id", runID, "profile_dir", profileDir, "path", path, "error", err)
				continue
			}
			if trimmed := strings.TrimSpace(string(content)); trimmed != "" {
				sections = append(sections, "# "+path+"\n"+trimmed)
			}
		}
	}
	return strings.Join(sections, "\n\n")
}

// joinInstructionSections concatenates non-empty instruction blocks (profile
// first, then repo) with the same blank-line separator loadTrustedStepInstructions
// uses between sections.
func joinInstructionSections(sections ...string) string {
	var nonEmpty []string
	for _, s := range sections {
		if strings.TrimSpace(s) != "" {
			nonEmpty = append(nonEmpty, s)
		}
	}
	return strings.Join(nonEmpty, "\n\n")
}

// profileStamp returns the run-record stamp identifying which profile revision
// gated a run: "<name>@<ref>" where ref is the profile checkout's HEAD SHA when
// the profile dir is a git repo, else a content hash of profile.yaml. The stamp
// lets a consumer (e.g. a fleet checker) confirm which profile enforced a gate.
func profileStamp(ctx context.Context, name, profileDir string, profileFile string) string {
	if sha, err := git.ResolveRef(ctx, profileDir, "HEAD"); err == nil && strings.TrimSpace(sha) != "" {
		return name + "@" + shortHash(strings.TrimSpace(sha))
	}
	if data, err := os.ReadFile(profileFile); err == nil {
		sum := sha256.Sum256(data)
		return name + "@sha256:" + shortHash(hex.EncodeToString(sum[:]))
	}
	return name
}

// shortHash trims a hex hash/SHA to a stable 12-char prefix for compact stamps.
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// HandlePushReceived processes a push notification from the post-receive hook.
// It creates a run, sets up a worktree, and launches pipeline execution in the background.
func (m *RunManager) HandlePushReceived(ctx context.Context, params *ipc.PushReceivedParams) (string, error) {
	// Ref deletion (git push remote :branch) sends new SHA as all-zeros.
	// Nothing to validate - skip pipeline.
	if git.IsZeroSHA(params.New) {
		return "", fmt.Errorf("ref deletion push, no pipeline to run")
	}

	repoID, err := repoIDFromGatePath(params.Gate)
	if err != nil {
		return "", err
	}

	repo, err := m.db.GetRepo(repoID)
	if err != nil {
		return "", fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return "", fmt.Errorf("unknown repo for gate %s", params.Gate)
	}

	branch := branchFromRef(params.Ref)
	return m.startRun(ctx, repo, branch, params.New, params.Old, "push", params.SkipSteps, params.Intent)
}

// HandleRerun creates a new run for the latest gate head on a branch. An
// optional intent is stamped onto the new run.
func (m *RunManager) HandleRerun(ctx context.Context, repoID, branch string, skipSteps []types.StepName, intent string) (string, error) {
	repo, err := m.db.GetRepo(repoID)
	if err != nil {
		return "", fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return "", fmt.Errorf("unknown repo %s", repoID)
	}

	gateDir := m.paths.RepoDir(repo.ID)
	headSHA, err := git.Run(ctx, gateDir, "rev-parse", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve gate head: %w", err)
	}

	runs, err := m.db.GetRunsByRepo(repoID)
	if err != nil {
		return "", fmt.Errorf("get runs: %w", err)
	}

	var latestForBranch *db.Run
	var matchingHead *db.Run
	for _, run := range runs {
		if run.Branch != branch {
			continue
		}
		if latestForBranch == nil {
			latestForBranch = run
		}
		if run.HeadSHA == headSHA {
			matchingHead = run
			break
		}
	}
	if latestForBranch == nil {
		return "", fmt.Errorf("no previous run for branch %s", branch)
	}

	baseSHA := latestForBranch.BaseSHA
	if matchingHead != nil {
		baseSHA = matchingHead.BaseSHA
	}

	return m.startRun(ctx, repo, branch, headSHA, baseSHA, "rerun", skipSteps, intent)
}

// startRun creates a run, sets up a worktree, and launches pipeline execution.
// A non-empty intent is stamped onto the run as agent-supplied, so the intent
// step uses it instead of inferring from transcripts.
func (m *RunManager) startRun(ctx context.Context, repo *db.Repo, branch, headSHA, baseSHA, trigger string, skipSteps []types.StepName, intent string) (string, error) {
	branchRole := telemetryBranchRole(branch, repo.DefaultBranch)
	trackStartFailure := func(stage string) {
		telemetry.Track("run", telemetry.Fields{
			"action":      "start_failed",
			"trigger":     trigger,
			"branch_role": branchRole,
			"stage":       stage,
		})
	}

	if m.shuttingDown.Load() {
		trackStartFailure("daemon_shutdown")
		return "", fmt.Errorf("daemon is shutting down")
	}

	// Serialize per repo+branch to prevent two concurrent pushes from both
	// passing cancelActiveRuns and creating duplicate pipelines.
	lockKey := repo.ID + "/" + branch
	lockVal, _ := m.branchLocks.LoadOrStore(lockKey, &sync.Mutex{})
	branchMu := lockVal.(*sync.Mutex)
	branchMu.Lock()
	defer branchMu.Unlock()

	// Cancel any active run for this repo+branch.
	m.cancelActiveRuns(repo.ID, branch)

	// Create run record.
	run, err := m.db.InsertRun(repo.ID, branch, headSHA, baseSHA)
	if err != nil {
		trackStartFailure("create_run")
		return "", fmt.Errorf("create run: %w", err)
	}

	// Stamp an agent-supplied intent onto the run before the pipeline starts,
	// so the intent step finds it already present and skips transcript-based
	// inference. A persist failure is non-fatal: the intent step would simply
	// fall back to inference.
	if trimmed := strings.TrimSpace(intent); trimmed != "" {
		if err := m.db.UpdateRunIntent(run.ID, db.RunIntent{Summary: trimmed, Source: "agent", Score: 1}); err != nil {
			slog.Warn("failed to persist agent-supplied intent", "run_id", run.ID, "error", err)
		} else {
			run.Intent = &trimmed
			source := "agent"
			run.IntentSource = &source
			score := 1.0
			run.IntentScore = &score
		}
	}

	// Create worktree from the gate bare repo.
	gateDir := m.paths.RepoDir(repo.ID)
	wtDir := m.paths.WorktreeDir(repo.ID, run.ID)
	if err := git.WorktreeAdd(ctx, gateDir, wtDir, headSHA); err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("create worktree: %s", err))
		trackStartFailure("create_worktree")
		return "", fmt.Errorf("create worktree: %w", err)
	}
	if err := git.CopyLocalUserIdentity(ctx, repo.WorkingPath, wtDir); err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("configure worktree git identity: %s", err))
		trackStartFailure("configure_worktree_identity")
		return "", fmt.Errorf("configure worktree git identity: %w", err)
	}
	// Fetch the trusted default branch and resolve it to an exact commit SHA
	// before any read. Reading the trusted config at this pinned SHA (rather
	// than the origin/<defaultBranch> remote-tracking ref) is what makes a
	// fetch failure fail closed: if the fetch errors or the ref does not
	// resolve, trustedSHA stays empty, loadTrustedRepoConfig returns nil, and
	// EffectiveRepoConfig drops the pushed branch's commands/agent. Without
	// the resolve, a stale origin/<defaultBranch> left in the shared bare
	// repo by a previous run could serve a trusted copy that the live default
	// branch has already removed — silently running stale shell.
	var trustedSHA string
	if repo.DefaultBranch != "" {
		if err := git.FetchRemoteBranch(ctx, wtDir, "origin", repo.DefaultBranch); err != nil {
			slog.Warn("failed to fetch default branch into worktree; trusted config disabled (commands/agent from pushed branch will be dropped)", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else if sha, err := git.ResolveRef(ctx, wtDir, "refs/remotes/origin/"+repo.DefaultBranch); err != nil {
			slog.Warn("failed to resolve fetched default-branch ref; trusted config disabled", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else {
			trustedSHA = sha
		}
	}

	// Track whether the background goroutine takes ownership of worktree cleanup.
	// If setup fails before the goroutine launches, we must clean up here.
	bgOwnsWorktree := false
	defer func() {
		if !bgOwnsWorktree {
			if rmErr := git.WorktreeRemove(context.Background(), gateDir, wtDir); rmErr != nil {
				slog.Warn("failed to remove worktree during setup cleanup", "path", wtDir, "error", rmErr)
			}
		}
	}()

	globalCfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("load config: %s", err))
		trackStartFailure("load_global_config")
		return "", fmt.Errorf("load global config: %w", err)
	}
	repoCfg, err := config.LoadRepo(wtDir)
	if err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("load config: %s", err))
		trackStartFailure("load_repo_config")
		return "", fmt.Errorf("load repo config: %w", err)
	}
	// SECURITY: load the code-executing selection fields (commands.*, agent,
	// and steps) from the trusted default-branch copy of .no-mistakes.yaml
	// rather than the pushed SHA. The worktree is checked out at headSHA (the
	// contributor's branch), so reading repoCfg above would honor a
	// contributor's commands/agent/steps and let any pushed SHA run arbitrary
	// shell (sh -c), pick the launched agent (incl. acp: targets), or drop
	// validation steps on the daemon host with the maintainer's env
	// (GH_TOKEN, SSH agent, ...). EffectiveRepoConfig replaces commands +
	// agent + steps with the trusted default-branch values unless the
	// maintainer has explicitly opted in.
	//
	// allow_repo_commands is itself read ONLY from the trusted copy: a
	// contributor cannot self-enable it from the pushed branch. With no
	// trusted copy (fetch failed, no default branch, or no file on it) the
	// opt-in is false and commands/agent are forced empty — fail closed.
	trustedRepoCfg := loadTrustedRepoConfig(ctx, wtDir, trustedSHA, run.ID)
	allowRepoCommands := trustedRepoCfg != nil && trustedRepoCfg.AllowRepoCommands
	effectiveRepoCfg := config.EffectiveRepoConfig(repoCfg, trustedRepoCfg, allowRepoCommands)
	if allowRepoCommands {
		slog.Warn("allow_repo_commands is enabled on the default branch: honoring commands/agent/steps from pushed branch", "run_id", run.ID, "branch", branch)
	} else if repoCfg.Commands != effectiveRepoCfg.Commands || repoCfg.Agent != effectiveRepoCfg.Agent || !config.StepSpecsEqual(repoCfg.Steps, effectiveRepoCfg.Steps) {
		// Surface the silent override so a maintainer who shipped a commands.*,
		// agent, or steps change on a feature branch understands why it did not
		// run. This is not an error: it is the secure default in action.
		slog.Info("repo commands/agent/steps loaded from default branch, not pushed branch", "run_id", run.ID, "branch", branch, "default_branch", repo.DefaultBranch)
	}
	cfg := config.Merge(globalCfg, effectiveRepoCfg)
	// Resolve the repo's own step instructions + skill bodies at the trusted
	// default-branch SHA (not the pushed worktree) so a contributor's branch
	// cannot rewrite the guidance/prompt that drives the maintainer's agent.
	repoInstructions := loadTrustedStepInstructions(ctx, wtDir, trustedSHA, cfg.Steps, run.ID)
	repoSteps := loadTrustedSkillBodies(ctx, wtDir, trustedSHA, cfg.Steps, run.ID)

	// Shared gate profile (trusted-only field). When selected, its steps —
	// resolved profile-relative from <NM_HOME>/profiles/<name>/ (host-local
	// disk, never the worktree) — compose with the repo's steps and become the
	// pipeline. A missing/unparsable profile, or a bad composition, fails the
	// run at start: a team gate must never silently drop to the default
	// pipeline.
	if profileName := strings.TrimSpace(effectiveRepoCfg.Profile); profileName != "" {
		profile, profileDir, err := m.loadProfile(profileName)
		if err != nil {
			m.db.UpdateRunError(run.ID, fmt.Sprintf("load profile: %s", err))
			trackStartFailure("load_profile")
			return "", fmt.Errorf("load profile %q: %w", profileName, err)
		}
		profileSteps := loadProfileSkillBodies(profileDir, profile.Steps, run.ID)
		profileInstructions := loadProfileStepInstructions(profileDir, profile.Steps, run.ID)
		mergedSteps, err := config.ComposeProfileSteps(repoSteps, profileSteps)
		if err != nil {
			m.db.UpdateRunError(run.ID, fmt.Sprintf("compose profile steps: %s", err))
			trackStartFailure("compose_profile")
			return "", fmt.Errorf("compose profile %q: %w", profileName, err)
		}
		cfg.Steps = mergedSteps
		cfg.StepInstructions = joinInstructionSections(profileInstructions, repoInstructions)
		stamp := profileStamp(ctx, profileName, profileDir, m.paths.ProfileFile(profileName))
		if err := m.db.SetRunProfile(run.ID, stamp); err != nil {
			slog.Warn("failed to stamp run profile", "run_id", run.ID, "error", err)
		} else {
			profileCopy := stamp
			run.Profile = &profileCopy
		}
		slog.Info("run gated by shared profile", "run_id", run.ID, "profile", stamp, "profile_dir", profileDir, "profile_version", profile.Version, "branch", branch)
	} else {
		// No profile selected: a `- use: profile` splice sentinel is meaningless
		// and must not silently pass through as a nameless step.
		if config.HasProfileSplice(repoSteps) {
			err := fmt.Errorf("repo steps: use `- use: profile` but no profile is selected on the trusted default branch")
			m.db.UpdateRunError(run.ID, err.Error())
			trackStartFailure("compose_profile")
			return "", err
		}
		cfg.Steps = repoSteps
		cfg.StepInstructions = repoInstructions
	}

	// Create agent. In demo mode, skip resolution and use a no-op agent.
	var ag agent.Agent
	if steps.IsDemoMode() {
		ag = agent.NewNoop()
	} else {
		if err := cfg.ResolveAgent(ctx, exec.LookPath); err != nil {
			m.db.UpdateRunError(run.ID, err.Error())
			trackStartFailure("resolve_agent")
			return "", err
		}
		var agErr error
		ag, agErr = agent.NewWithOptions(cfg.Agent, cfg.AgentPath(), cfg.AgentArgs(), agent.Options{
			ACPRegistryOverrides: cfg.ACPRegistryOverrides,
		})
		if agErr != nil {
			m.db.UpdateRunError(run.ID, fmt.Sprintf("create agent: %s", agErr))
			trackStartFailure("create_agent")
			return "", fmt.Errorf("create agent: %w", agErr)
		}
		// Steer every pipeline agent to keep writes inside the worktree and
		// avoid mutating system state (e.g. brew/Homebrew touching
		// /Applications), which triggers macOS App Management prompts.
		ag = agent.WithSteering(ag)
	}

	execSteps, err := m.steps(cfg)
	if err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("build pipeline steps: %s", err))
		trackStartFailure("build_steps")
		return "", fmt.Errorf("build pipeline steps: %w", err)
	}
	telemetry.Track("run", telemetry.Fields{
		"action":      "started",
		"trigger":     trigger,
		"agent":       string(cfg.Agent),
		"branch_role": branchRole,
		"step_count":  len(execSteps),
		"demo_mode":   steps.IsDemoMode(),
	})

	// Create executor with event broadcast.
	runCtx, cancel := context.WithCancelCause(context.Background())
	executor := pipeline.NewExecutor(m.db, m.paths, cfg, ag, execSteps, m.broadcast)
	executor.SetSkippedSteps(skipSteps)

	// Track executor.
	done := make(chan struct{})
	m.mu.Lock()
	m.executors[run.ID] = executor
	m.cancels[run.ID] = cancel
	m.dones[run.ID] = done
	m.mu.Unlock()

	// Background goroutine now owns worktree cleanup.
	bgOwnsWorktree = true

	// Launch pipeline in background.
	m.wg.Add(1)
	go func() {
		startedAt := time.Now()
		defer m.wg.Done()
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				errMsg := fmt.Sprintf("internal panic: %v", r)
				slog.Error("panic in pipeline goroutine", "run_id", run.ID, "panic", r)
				run.Status = types.RunFailed
				run.Error = &errMsg
				fields := telemetry.Fields{
					"action":      "finished",
					"trigger":     trigger,
					"agent":       string(cfg.Agent),
					"branch_role": branchRole,
					"status":      string(run.Status),
					"duration_ms": time.Since(startedAt).Milliseconds(),
					"step_count":  len(execSteps),
					"pr_created":  run.PRURL != nil && *run.PRURL != "",
				}
				if failedStep := telemetryFailedStepName(m.db, run.ID); failedStep != "" {
					fields["failed_step"] = failedStep
				}
				telemetry.Track("run", fields)
				if dbErr := m.db.UpdateRunErrorStatus(run.ID, errMsg, types.RunFailed); dbErr != nil {
					slog.Error("failed to update run after panic", "run_id", run.ID, "error", dbErr)
				}
			}
			cancel(nil)
			ag.Close()
			// Close subscriber channels for this run.
			m.closeSubscribers(run.ID)
			// Clean up worktree.
			if rmErr := git.WorktreeRemove(context.Background(), gateDir, wtDir); rmErr != nil {
				slog.Warn("failed to remove worktree", "path", wtDir, "error", rmErr)
			}
			// Remove tracking.
			m.mu.Lock()
			delete(m.executors, run.ID)
			delete(m.cancels, run.ID)
			delete(m.dones, run.ID)
			m.mu.Unlock()
		}()

		if err := executor.Execute(runCtx, run, repo, wtDir); err != nil {
			fields := telemetry.Fields{
				"action":      "finished",
				"trigger":     trigger,
				"agent":       string(cfg.Agent),
				"branch_role": branchRole,
				"status":      string(run.Status),
				"duration_ms": time.Since(startedAt).Milliseconds(),
				"step_count":  len(execSteps),
				"pr_created":  run.PRURL != nil && *run.PRURL != "",
			}
			if failedStep := telemetryFailedStepName(m.db, run.ID); failedStep != "" {
				fields["failed_step"] = failedStep
			}
			telemetry.Track("run", fields)
			slog.Error("pipeline failed", "run_id", run.ID, "error", err)
		} else {
			telemetry.Track("run", telemetry.Fields{
				"action":      "finished",
				"trigger":     trigger,
				"agent":       string(cfg.Agent),
				"branch_role": branchRole,
				"status":      string(run.Status),
				"duration_ms": time.Since(startedAt).Milliseconds(),
				"step_count":  len(execSteps),
				"pr_created":  run.PRURL != nil && *run.PRURL != "",
			})
			slog.Info("pipeline completed", "run_id", run.ID)
		}
	}()

	return run.ID, nil
}

func telemetryBranchRole(branch, defaultBranch string) string {
	if branch == "" {
		return "unknown"
	}
	if defaultBranch != "" && branch == defaultBranch {
		return "default"
	}
	return "feature"
}

func telemetryFailedStepName(database *db.DB, runID string) string {
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return ""
	}
	for _, step := range steps {
		if step.Status == types.StepStatusFailed {
			return string(step.StepName)
		}
	}
	return ""
}

// HandleRespond routes a user approval action to the executor for the given run.
func (m *RunManager) HandleRespond(runID string, step types.StepName, action types.ApprovalAction, findingIDs []string) error {
	return m.HandleRespondWithOverrides(runID, step, action, findingIDs, nil, nil)
}

// HandleRespondWithOverrides is like HandleRespond but also forwards user
// instructions and user-authored findings to the executor.
func (m *RunManager) HandleRespondWithOverrides(runID string, step types.StepName, action types.ApprovalAction, findingIDs []string, instructions map[string]string, addedFindings []types.Finding) error {
	m.mu.Lock()
	exec, ok := m.executors[runID]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("no active executor for run %s", runID)
	}

	return exec.RespondWithOverrides(step, action, findingIDs, instructions, addedFindings)
}

// Shutdown cancels all active runs. Called during daemon shutdown to prevent
// orphaned goroutines from continuing agent calls and git operations.
func (m *RunManager) Shutdown() {
	m.shuttingDown.Store(true)

	m.mu.Lock()
	cancels := make(map[string]context.CancelCauseFunc, len(m.cancels))
	for id, cancel := range m.cancels {
		cancels[id] = cancel
	}
	m.mu.Unlock()

	for id, cancel := range cancels {
		cancel(fmt.Errorf("daemon shutting down"))
		slog.Info("cancelled run on shutdown", "run_id", id)
	}

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		slog.Warn("timed out waiting for runs to finish during shutdown")
	}
}

// HandleCancel stops an active run and propagates cancellation to the executor.
func (m *RunManager) HandleCancel(runID string) error {
	m.mu.Lock()
	cancel, ok := m.cancels[runID]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("no active run %s", runID)
	}

	cancel(fmt.Errorf(types.RunCancelReasonAbortedByUser))
	return nil
}

// cancelActiveRuns cancels any in-progress runs for the given repo+branch
// and waits for their goroutines to finish before returning, preventing
// concurrent pushes to upstream.
// The cancellation cause is propagated to the executor via context.Cause,
// which uses it as the run's error message in the DB.
func (m *RunManager) cancelActiveRuns(repoID, branch string) {
	runs, err := m.db.GetRunsByRepo(repoID)
	if err != nil {
		slog.Error("failed to query active runs for cancellation", "repo", repoID, "branch", branch, "error", err)
		return
	}

	var toWait []chan struct{}
	for _, run := range runs {
		if run.Branch != branch {
			continue
		}
		if run.Status != types.RunPending && run.Status != types.RunRunning {
			continue
		}

		m.mu.Lock()
		cancel, ok := m.cancels[run.ID]
		done := m.dones[run.ID]
		m.mu.Unlock()
		if !ok {
			continue
		}

		cancel(fmt.Errorf(types.RunCancelReasonSuperseded))
		slog.Info("cancelled active run", "run_id", run.ID, "repo_id", repoID, "branch", branch)
		if done != nil {
			toWait = append(toWait, done)
		}
	}

	timeout := time.After(30 * time.Second)
	for _, done := range toWait {
		select {
		case <-done:
		case <-timeout:
			slog.Warn("timed out waiting for cancelled runs to finish")
			return
		}
	}
}
