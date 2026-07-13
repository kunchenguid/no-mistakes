package daemon

import (
	"context"
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

// StepFactory creates pipeline steps for a run. Defaults to steps.AllSteps.
type StepFactory func() []pipeline.Step

var recoveredConfigFetchTimeout = 10 * time.Second

var fetchRecoveredRemoteBranch = git.FetchRemoteBranch

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
		stepFactory = func() []pipeline.Step { return steps.AllSteps() }
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

type recoveredRunPlan struct {
	run     *db.Run
	repo    *db.Repo
	workDir string
	gateDir string
	cfg     *config.Config
	agent   agent.Agent
	steps   []pipeline.Step
}

func (m *RunManager) recoverableParkedRuns(ctx context.Context) []recoveredRunPlan {
	runs, err := m.db.GetActiveRuns()
	if err != nil {
		slog.Error("failed to list active runs for recovery", "error", err)
		return nil
	}
	plans := make([]recoveredRunPlan, 0, len(runs))
	branchCounts := make(map[string]int, len(runs))
	for _, run := range runs {
		branchCounts[run.RepoID+"\x00"+run.Branch]++
	}
	for _, run := range runs {
		if branchCounts[run.RepoID+"\x00"+run.Branch] != 1 {
			slog.Warn("active run cannot be safely resumed", "run_id", run.ID, "error", "conflicting active run for branch")
			continue
		}
		plan, err := m.prepareRecoveredRun(ctx, run)
		if err != nil {
			slog.Warn("active run cannot be safely resumed", "run_id", run.ID, "error", err)
			continue
		}
		plans = append(plans, *plan)
	}
	return plans
}

func (m *RunManager) prepareRecoveredRun(ctx context.Context, run *db.Run) (*recoveredRunPlan, error) {
	if run == nil || run.Status != types.RunRunning || run.AwaitingAgentSince == nil || run.Branch == "" {
		return nil, fmt.Errorf("run is not a parked running run")
	}
	repo, err := m.db.GetRepo(run.RepoID)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return nil, fmt.Errorf("run repository is missing")
	}
	workDir := m.paths.WorktreeDir(repo.ID, run.ID)
	if info, err := os.Stat(workDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("worktree is missing")
	}
	headSHA, err := git.HeadSHA(ctx, workDir)
	if err != nil || headSHA != run.HeadSHA {
		return nil, fmt.Errorf("worktree head does not match run head")
	}
	gateDir := m.paths.RepoDir(repo.ID)
	commonDir, err := git.Run(ctx, workDir, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("resolve worktree common git dir: %w", err)
	}
	if !samePath(resolveGitPath(workDir, commonDir), gateDir) {
		return nil, fmt.Errorf("worktree does not belong to its gate repository")
	}

	execSteps := m.steps()
	if err := pipeline.ValidateRecoveredRun(m.db, run, execSteps); err != nil {
		return nil, err
	}
	cfg, err := m.loadRecoveredConfig(ctx, run, repo, workDir)
	if err != nil {
		return nil, err
	}
	ag, err := newPipelineAgent(ctx, cfg, exec.LookPath)
	if err != nil {
		return nil, err
	}
	if cfg.SessionReuse {
		if err := validateRecoveredSessionProviders(m.db, run.ID, ag); err != nil {
			_ = ag.Close()
			return nil, err
		}
	}
	return &recoveredRunPlan{
		run:     run,
		repo:    repo,
		workDir: workDir,
		gateDir: gateDir,
		cfg:     cfg,
		agent:   ag,
		steps:   execSteps,
	}, nil
}

func validateRecoveredSessionProviders(database *db.DB, runID string, ag agent.Agent) error {
	sessions, err := database.GetRunAgentSessions(runID)
	if err != nil {
		return fmt.Errorf("get run sessions: %w", err)
	}
	for _, session := range sessions {
		if session.Role != string(pipeline.SessionRoleReviewer) && session.Role != string(pipeline.SessionRoleFixer) {
			return fmt.Errorf("recovered run has unknown session role %q", session.Role)
		}
		if session.Agent == "" || session.SessionID == "" {
			return fmt.Errorf("recovered run has incomplete session metadata")
		}
		if !agent.SupportsSessionProvider(ag, session.Agent) {
			return fmt.Errorf("session provider %q is no longer configured", session.Agent)
		}
	}
	return nil
}

func (m *RunManager) loadRecoveredConfig(ctx context.Context, run *db.Run, repo *db.Repo, workDir string) (*config.Config, error) {
	globalCfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		return nil, fmt.Errorf("load global config: %w", err)
	}
	repoCfg, err := config.LoadRepo(workDir)
	if err != nil {
		return nil, fmt.Errorf("load repo config: %w", err)
	}
	var trustedSHA string
	if repo.DefaultBranch != "" {
		fetchCtx, cancel := context.WithTimeout(ctx, recoveredConfigFetchTimeout)
		defer cancel()
		if err := fetchRecoveredRemoteBranch(fetchCtx, workDir, "origin", repo.DefaultBranch); err != nil {
			slog.Warn("failed to fetch default branch while recovering run; trusted config disabled", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else if sha, err := git.ResolveRef(ctx, workDir, "refs/remotes/origin/"+repo.DefaultBranch); err != nil {
			slog.Warn("failed to resolve default branch while recovering run; trusted config disabled", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else {
			trustedSHA = sha
		}
	}
	// SECURITY: a trusted-config fetch failure must abort, not silently disable
	// the disable_project_settings opt-out (see assertGateTrustedConfigReadable).
	if err := assertGateTrustedConfigReadable(ctx, workDir, repo.DefaultBranch, trustedSHA); err != nil {
		return nil, err
	}
	trustedRepoCfg := loadTrustedRepoConfig(ctx, workDir, trustedSHA, run.ID)
	allowRepoCommands := trustedRepoCfg != nil && trustedRepoCfg.AllowRepoCommands
	return config.Merge(globalCfg, config.EffectiveRepoConfig(repoCfg, trustedRepoCfg, allowRepoCommands)), nil
}

func newPipelineAgent(ctx context.Context, cfg *config.Config, lookPath func(string) (string, error)) (agent.Agent, error) {
	if steps.IsDemoMode() {
		return agent.NewNoop(), nil
	}
	if err := cfg.ResolveAgent(ctx, lookPath); err != nil {
		return nil, err
	}
	agents := cfg.Agents
	if len(agents) == 0 {
		agents = []types.AgentName{cfg.Agent}
	}
	created := make([]agent.Agent, 0, len(agents))
	for _, name := range agents {
		next, err := agent.NewWithOptions(name, cfg.AgentPathFor(name), cfg.AgentArgsFor(name), agent.Options{
			ACPRegistryOverrides:   cfg.ACPRegistryOverrides,
			DisableProjectSettings: cfg.DisableProjectSettings,
		})
		if err != nil {
			for _, existing := range created {
				_ = existing.Close()
			}
			return nil, fmt.Errorf("create agent %s: %w", name, err)
		}
		created = append(created, agent.WithSteering(next))
	}
	ag := agent.NewFallback(created)
	// Fail closed ONLY under the trusted opt-out (see startRun): refuse an
	// unverified harness when the repo disabled project settings; otherwise run
	// every adapter as before.
	if cfg.DisableProjectSettings {
		if err := agent.EnsureGateNeutralized(ag); err != nil {
			_ = ag.Close()
			return nil, err
		}
	}
	return ag, nil
}

func resolveGitPath(workDir, value string) string {
	value = strings.TrimSpace(value)
	if !filepath.IsAbs(value) {
		value = filepath.Join(workDir, value)
	}
	return filepath.Clean(value)
}

func samePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	}
	return a == b
}

func (m *RunManager) resumeRecoveredRuns(plans []recoveredRunPlan) {
	for _, plan := range plans {
		m.resumeRecoveredRun(plan)
	}
}

func (m *RunManager) resumeRecoveredRun(plan recoveredRunPlan) {
	if m.shuttingDown.Load() {
		_ = plan.agent.Close()
		return
	}
	runCtx, cancel := context.WithCancelCause(context.Background())
	executor := pipeline.NewExecutor(m.db, m.paths, plan.cfg, plan.agent, plan.steps, m.broadcast)
	done := make(chan struct{})
	m.mu.Lock()
	m.executors[plan.run.ID] = executor
	m.cancels[plan.run.ID] = cancel
	m.dones[plan.run.ID] = done
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		startedAt := time.Now()
		defer m.wg.Done()
		defer close(done)
		defer func() {
			if recovered := recover(); recovered != nil {
				errMsg := fmt.Sprintf("internal panic: %v", recovered)
				plan.run.Status = types.RunFailed
				plan.run.Error = &errMsg
				if err := m.db.UpdateRunErrorStatus(plan.run.ID, errMsg, types.RunFailed); err != nil {
					slog.Error("failed to update recovered run after panic", "run_id", plan.run.ID, "error", err)
				}
			}
			cancel(nil)
			_ = plan.agent.Close()
			m.closeSubscribers(plan.run.ID)
			if err := git.WorktreeRemove(context.Background(), plan.gateDir, plan.workDir); err != nil {
				slog.Warn("failed to remove recovered worktree", "path", plan.workDir, "error", err)
			}
			m.mu.Lock()
			delete(m.executors, plan.run.ID)
			delete(m.cancels, plan.run.ID)
			delete(m.dones, plan.run.ID)
			m.mu.Unlock()
		}()

		if err := executor.Resume(runCtx, plan.run, plan.repo, plan.workDir); err != nil {
			if plan.run.Status == types.RunRunning {
				errMsg := err.Error()
				plan.run.Status = types.RunFailed
				plan.run.Error = &errMsg
				if dbErr := m.db.UpdateRunErrorStatus(plan.run.ID, errMsg, types.RunFailed); dbErr != nil {
					slog.Error("failed to mark recovered run failed", "run_id", plan.run.ID, "error", dbErr)
				}
			}
			slog.Error("recovered pipeline failed", "run_id", plan.run.ID, "error", err)
		}
		fields := telemetry.Fields{
			"action":      "finished",
			"trigger":     "recovery",
			"agent":       string(plan.cfg.Agent),
			"branch_role": telemetryBranchRole(plan.run.Branch, plan.repo.DefaultBranch),
			"status":      string(plan.run.Status),
			"duration_ms": time.Since(startedAt).Milliseconds(),
			"step_count":  len(plan.steps),
			"pr_created":  plan.run.PRURL != nil && *plan.run.PRURL != "",
		}
		if failedStep := telemetryFailedStepName(m.db, plan.run.ID); failedStep != "" {
			fields["failed_step"] = failedStep
		}
		addRunPerformanceSummary(m.db, plan.run.ID, fields)
		telemetry.Track("run", fields)
	}()
}

func agentListsEqual(a, b []types.AgentName) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
// default-branch commit (trustedSHA - the exact SHA startRun just fetched and
// resolved) in the worktree and parses it. Reading at a pinned SHA, rather
// than the origin/<defaultBranch> remote-tracking ref, closes the stale-ref
// hole: the gate worktree shares refs with the bare repo, so without a fresh
// fetch + resolve the ref could point at a commit a previous run left behind.
//
// trustedSHA is empty when the default branch is unknown, the fetch failed,
// or the ref did not resolve. The caller must first reject those cases with
// assertGateTrustedConfigReadable; returning nil here remains defensive and
// ensures EffectiveRepoConfig never uses pushed gate-control fields.
func loadTrustedRepoConfig(ctx context.Context, wtDir, trustedSHA, runID string) *config.RepoConfig {
	if trustedSHA == "" {
		// No trusted SHA means no freshly-fetched default-branch commit to
		// read from. Return nil so EffectiveRepoConfig forces empty
		// commands/agent - the secure default - instead of falling back to a
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

// assertGateTrustedConfigReadable fails a run LOUD when the trusted
// default-branch copy of .no-mistakes.yaml could not be READ at all. This is the
// security correction for disable_project_settings: that field is a boundary
// honored only from the trusted copy, so an unreadable trusted config must NOT
// be silently treated as "not opted out" - no-mistakes cannot know whether the
// repo relies on the boundary, so it refuses to run rather than risk launching a
// gate agent with the project instructions loaded.
//
// It distinguishes "could not read the trusted config at all" (abort) from
// "read the trusted tree fine, there is simply no .no-mistakes.yaml on the
// default branch" (the common ordinary-repo case, which is NOT opted out and
// must proceed). Abort cases:
//   - no known default branch to read a trusted copy from,
//   - the default branch could not be fetched/resolved to a pinned SHA,
//   - the pinned commit or tree is not readable (missing object / partial fetch),
//   - the trusted .no-mistakes.yaml is present but unreadable or unparseable.
func assertGateTrustedConfigReadable(ctx context.Context, wtDir, defaultBranch, trustedSHA string) error {
	if defaultBranch == "" {
		return fmt.Errorf("cannot evaluate disable_project_settings: repository has no known default branch to read trusted config from")
	}
	if trustedSHA == "" {
		return fmt.Errorf("cannot evaluate disable_project_settings: failed to fetch or resolve trusted default branch %q (refusing to run without reading the trusted config)", defaultBranch)
	}
	if _, err := git.Run(ctx, wtDir, "rev-parse", "-q", "--verify", trustedSHA+"^{commit}"); err != nil {
		return fmt.Errorf("cannot evaluate disable_project_settings: trusted default-branch commit %s is not readable: %w", trustedSHA, err)
	}
	entry, err := git.Run(ctx, wtDir, "ls-tree", trustedSHA, "--", ".no-mistakes.yaml")
	if err != nil {
		return fmt.Errorf("cannot evaluate disable_project_settings: trusted default-branch tree at %s is not readable: %w", trustedSHA, err)
	}
	if entry == "" {
		return nil
	}
	content, err := git.ShowFile(ctx, wtDir, trustedSHA, ".no-mistakes.yaml")
	if err != nil {
		return fmt.Errorf("cannot evaluate disable_project_settings: trusted .no-mistakes.yaml at %s is present but not readable: %w", trustedSHA, err)
	}
	if _, err := config.LoadRepoFromBytes([]byte(content)); err != nil {
		return fmt.Errorf("cannot evaluate disable_project_settings: trusted .no-mistakes.yaml at %s is present but unparseable: %w", trustedSHA, err)
	}
	return nil
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
		if err := m.db.UpdateRunIntent(run.ID, db.RunIntent{Summary: trimmed, Source: db.RunIntentSourceAgent, Score: 1}); err != nil {
			slog.Warn("failed to persist agent-supplied intent", "run_id", run.ID, "error", err)
		} else {
			run.Intent = &trimmed
			source := db.RunIntentSourceAgent
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
	// branch has already removed - silently running stale shell.
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
	// SECURITY: load the code-executing selection fields (commands.* and
	// agent) from the trusted default-branch copy of .no-mistakes.yaml rather
	// than the pushed SHA. The worktree is checked out at headSHA (the
	// contributor's branch), so reading repoCfg above would honor a
	// contributor's commands/agent and let any pushed SHA run arbitrary shell
	// (sh -c) or pick the launched agent (incl. acp: targets) on the daemon
	// host with the maintainer's env (GH_TOKEN, SSH agent, ...).
	// EffectiveRepoConfig replaces commands + agent with the trusted
	// default-branch values unless the maintainer has explicitly opted in.
	//
	// allow_repo_commands is itself read ONLY from the trusted copy: a
	// contributor cannot self-enable it from the pushed branch. A readable
	// trusted tree with no config leaves the opt-in false and forces
	// commands/agent empty. An unreadable trusted tree aborts below.
	// SECURITY: a trusted-config fetch failure must abort, not silently disable
	// the disable_project_settings opt-out (see assertGateTrustedConfigReadable).
	if err := assertGateTrustedConfigReadable(ctx, wtDir, repo.DefaultBranch, trustedSHA); err != nil {
		m.db.UpdateRunError(run.ID, err.Error())
		trackStartFailure("trusted_config_unreadable")
		return "", err
	}
	trustedRepoCfg := loadTrustedRepoConfig(ctx, wtDir, trustedSHA, run.ID)
	allowRepoCommands := trustedRepoCfg != nil && trustedRepoCfg.AllowRepoCommands
	effectiveRepoCfg := config.EffectiveRepoConfig(repoCfg, trustedRepoCfg, allowRepoCommands)
	if allowRepoCommands {
		slog.Warn("allow_repo_commands is enabled on the default branch: honoring commands/agent from pushed branch", "run_id", run.ID, "branch", branch)
	} else if repoCfg.Commands != effectiveRepoCfg.Commands || repoCfg.Agent != effectiveRepoCfg.Agent || !agentListsEqual(repoCfg.Agents, effectiveRepoCfg.Agents) {
		// Surface the silent override so a maintainer who shipped a commands.*
		// or agent change on a feature branch understands why it did not run.
		// This is not an error: it is the secure default in action.
		slog.Info("repo commands/agent loaded from default branch, not pushed branch", "run_id", run.ID, "branch", branch, "default_branch", repo.DefaultBranch)
	}
	cfg := config.Merge(globalCfg, effectiveRepoCfg)

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
		agents := cfg.Agents
		if len(agents) == 0 {
			agents = []types.AgentName{cfg.Agent}
		}
		created := make([]agent.Agent, 0, len(agents))
		for _, name := range agents {
			next, agErr := agent.NewWithOptions(name, cfg.AgentPathFor(name), cfg.AgentArgsFor(name), agent.Options{
				ACPRegistryOverrides:   cfg.ACPRegistryOverrides,
				DisableProjectSettings: cfg.DisableProjectSettings,
			})
			if agErr != nil {
				m.db.UpdateRunError(run.ID, fmt.Sprintf("create agent %s: %s", name, agErr))
				trackStartFailure("create_agent")
				return "", fmt.Errorf("create agent %s: %w", name, agErr)
			}
			// Steer every pipeline agent to keep writes inside the worktree and
			// avoid mutating system state (e.g. brew/Homebrew touching
			// /Applications), which triggers macOS App Management prompts.
			created = append(created, agent.WithSteering(next))
		}
		ag = agent.NewFallback(created)
		// Fail closed ONLY under the trusted opt-out: when the repo asked to
		// disable project settings, refuse any resolved harness that lacks a
		// verified suppression knob rather than launch it with the target repo's
		// project instructions loaded. When the repo did not opt out, every
		// adapter runs exactly as before (backward-compat).
		if cfg.DisableProjectSettings {
			if err := agent.EnsureGateNeutralized(ag); err != nil {
				m.db.UpdateRunError(run.ID, err.Error())
				trackStartFailure("gate_not_neutralized")
				return "", err
			}
		}
	}

	execSteps := m.steps()
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
				addRunPerformanceSummary(m.db, run.ID, fields)
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
			addRunPerformanceSummary(m.db, run.ID, fields)
			telemetry.Track("run", fields)
			slog.Error("pipeline failed", "run_id", run.ID, "error", err)
		} else {
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
			addRunPerformanceSummary(m.db, run.ID, fields)
			telemetry.Track("run", fields)
			slog.Info("pipeline completed", "run_id", run.ID)
		}
	}()

	return run.ID, nil
}

// addRunPerformanceSummary attaches the bounded per-run performance rollup
// to the terminal "run finished" event: low-cardinality counts only. The
// detailed per-invocation evidence (session keys, models, timings, tokens)
// stays in the local agent_invocations table and is never sent remotely.
func addRunPerformanceSummary(database *db.DB, runID string, fields telemetry.Fields) {
	summary, err := database.AgentInvocationSummaryForRun(runID)
	if err != nil {
		return
	}
	fields["agent_invocations"] = summary.Count
	fields["resumed_invocations"] = summary.Resumed
	fields["fallback_invocations"] = summary.Fallback
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
