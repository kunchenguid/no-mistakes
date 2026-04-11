package daemon

import (
	"context"
	"fmt"
	"log/slog"
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
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// StepFactory creates pipeline steps for a run. Defaults to steps.AllSteps.
type StepFactory func() []pipeline.Step

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
	return m.startRun(ctx, repo, branch, params.New, params.Old)
}

// HandleRerun creates a new run for the latest gate head on a branch.
func (m *RunManager) HandleRerun(ctx context.Context, repoID, branch string) (string, error) {
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

	return m.startRun(ctx, repo, branch, headSHA, baseSHA)
}

// startRun creates a run, sets up a worktree, and launches pipeline execution.
func (m *RunManager) startRun(ctx context.Context, repo *db.Repo, branch, headSHA, baseSHA string) (string, error) {
	if m.shuttingDown.Load() {
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
		return "", fmt.Errorf("create run: %w", err)
	}

	// Create worktree from the gate bare repo.
	gateDir := m.paths.RepoDir(repo.ID)
	wtDir := m.paths.WorktreeDir(repo.ID, run.ID)
	if err := git.WorktreeAdd(ctx, gateDir, wtDir, headSHA); err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("create worktree: %s", err))
		return "", fmt.Errorf("create worktree: %w", err)
	}
	if repo.DefaultBranch != "" {
		if err := git.FetchRemoteBranch(ctx, wtDir, "origin", repo.DefaultBranch); err != nil {
			slog.Warn("failed to fetch default branch into worktree", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
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

	// Load config.
	globalCfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("load config: %s", err))
		return "", fmt.Errorf("load global config: %w", err)
	}
	repoCfg, err := config.LoadRepo(wtDir)
	if err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("load config: %s", err))
		return "", fmt.Errorf("load repo config: %w", err)
	}
	cfg := config.Merge(globalCfg, repoCfg)

	// Create agent.
	ag, err := agent.New(cfg.Agent, cfg.AgentPath())
	if err != nil {
		m.db.UpdateRunError(run.ID, fmt.Sprintf("create agent: %s", err))
		return "", fmt.Errorf("create agent: %w", err)
	}

	// Create executor with event broadcast.
	runCtx, cancel := context.WithCancelCause(context.Background())
	executor := pipeline.NewExecutor(m.db, m.paths, cfg, ag, m.steps(), m.broadcast)

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
		defer m.wg.Done()
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in pipeline goroutine", "run_id", run.ID, "panic", r)
				m.db.UpdateRunError(run.ID, fmt.Sprintf("internal panic: %v", r))
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
			slog.Error("pipeline failed", "run_id", run.ID, "error", err)
		} else {
			slog.Info("pipeline completed", "run_id", run.ID)
		}
	}()

	return run.ID, nil
}

// HandleRespond routes a user approval action to the executor for the given run.
func (m *RunManager) HandleRespond(runID string, step types.StepName, action types.ApprovalAction, findingIDs []string) error {
	m.mu.Lock()
	exec, ok := m.executors[runID]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("no active executor for run %s", runID)
	}

	return exec.Respond(step, action, findingIDs)
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
