package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/logstore"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var applyShellEnvToProcess = shellenv.ApplyToProcess
var createDaemonPIDTempFile = os.CreateTemp
var renameDaemonPIDFile = os.Rename

// Run starts the daemon process. It blocks until a shutdown signal is received
// or the shutdown IPC method is called. This is called via the hidden
// `no-mistakes daemon run` entrypoint used by managed and detached services.
func Run() (retErr error) {
	startupStarted := time.Now()
	p, err := paths.New()
	if err != nil {
		return fmt.Errorf("resolve paths: %w", err)
	}
	if err := p.EnsureDirs(); err != nil {
		return fmt.Errorf("create directories: %w", err)
	}
	lock, err := acquireSingletonLock(p)
	if err != nil {
		return err
	}
	defer lock.Release()
	bootstrapCapture, err := startBootstrapCapture(p)
	if err != nil {
		return fmt.Errorf("capture daemon bootstrap log: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, bootstrapCapture.Close()) }()
	lifecycleLog, err := logstore.Open(p.DaemonLog(), logstore.LifecyclePolicy())
	if err != nil {
		return fmt.Errorf("open daemon lifecycle log: %w", err)
	}
	defer lifecycleLog.Close()
	initLogger(lifecycleLog, "info")
	defer func() {
		if retErr != nil {
			slog.Error("daemon failed", "error", retErr)
		}
	}()

	environmentStarted := time.Now()
	if err := prepareDaemonEnvironment(); err != nil {
		return err
	}
	logStartupPhase("environment", environmentStarted)

	// Ensure default config exists, then load it.
	config.EnsureDefaultGlobalConfig(p.ConfigFile())
	globalCfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	initLogger(lifecycleLog, globalCfg.LogLevel)

	databaseStarted := time.Now()
	d, err := db.Open(p.DB())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer d.Close()
	logStartupPhase("database", databaseStarted)

	return runWithOptionsLocked(p, d, nil, startupStarted)
}

func prepareDaemonEnvironment() error {
	nmHome := os.Getenv("NM_HOME")
	for _, key := range []string{
		"CLAUDECODE",
		"CLAUDE_CODE_ENTRYPOINT",
		"CLAUDE_CODE_ENTRY_POINT",
		"CLAUDE_CODE_SESSION_ID",
		"CLAUDE_CODE_SESSION_ACCESS_TOKEN",
	} {
		if err := os.Unsetenv(key); err != nil {
			return fmt.Errorf("unset %s: %w", key, err)
		}
	}
	if err := applyShellEnvToProcess(); err != nil {
		return fmt.Errorf("apply login shell environment: %w", err)
	}
	if nmHome != "" {
		if err := os.Setenv("NM_HOME", nmHome); err != nil {
			return fmt.Errorf("restore NM_HOME: %w", err)
		}
	}
	logDaemonPathSummary()
	return nil
}

// logDaemonPathSummary records the effective PATH at daemon startup so that
// "agent binary not in PATH" failures (see #143) can be diagnosed from the
// lifecycle log alone. The daemon installs its lifecycle handler at info
// before environment preparation, then reapplies the configured level after
// loading global config, so this startup diagnostic is always retained.
func logDaemonPathSummary() {
	path := os.Getenv("PATH")
	entries := 0
	if path != "" {
		entries = len(filepath.SplitList(path))
	}
	slog.Info("daemon environment ready",
		"path_entries", entries,
		"path", path,
	)
}

// initLogger sets up the global slog handler with the configured log level.
func initLogger(w io.Writer, level string) {
	lvl := config.ParseLogLevel(level)
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(handler))
}

func logStartupPhase(phase string, started time.Time, attrs ...any) {
	fields := []any{"phase", phase, "duration_ms", time.Since(started).Milliseconds()}
	fields = append(fields, attrs...)
	slog.Info("daemon startup phase complete", fields...)
}

// RunWithResources starts the daemon with pre-initialized paths and DB.
// Useful for testing where the caller controls resource setup.
func RunWithResources(p *paths.Paths, d *db.DB) error {
	return RunWithOptions(p, d, nil)
}

// RunWithOptions starts the daemon with optional overrides.
// stepFactory overrides the default pipeline steps (for testing).
func RunWithOptions(p *paths.Paths, d *db.DB, stepFactory StepFactory) error {
	startupStarted := time.Now()
	// Singleton guard: only one live daemon may own this NM_HOME at a time.
	// This must be acquired before recoverOnStartup (global stale-run
	// recovery and orphan-worktree cleanup) and before the IPC socket is
	// bound, and held for the rest of the process lifetime - otherwise a
	// second daemon racing to start against the same root can mark another
	// live daemon's active runs as crashed and delete worktrees out from
	// under it (see AGENTS.md "Daemon Singleton Lock").
	lock, err := acquireSingletonLock(p)
	if err != nil {
		return err
	}
	defer lock.Release()

	return runWithOptionsLocked(p, d, stepFactory, startupStarted)
}

func runWithOptionsLocked(p *paths.Paths, d *db.DB, stepFactory StepFactory, startupStarted time.Time) error {
	managedServerLog, err := logstore.Open(p.ManagedServerLog(), logstore.ManagedServerPolicy())
	if err != nil {
		return fmt.Errorf("open managed server log: %w", err)
	}
	agent.SetManagedServerOutput(managedServerLog)
	defer managedServerLog.Close()
	defer agent.SetManagedServerOutput(nil)

	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
		defer cancel()
		_ = telemetry.Close(ctx)
	}()

	// Point the agent package at our PID tracking dir so any managed
	// servers we spawn from here on leave crash-recovery breadcrumbs.
	agent.SetServerPIDsDir(p.ServerPIDsDir())
	defer agent.SetServerPIDsDir("")

	mgr := NewRunManager(d, p, stepFactory)

	// Publish process identity as soon as the singleton lock is held. Startup
	// callers can now distinguish a launched child from IPC readiness and detect
	// an early managed-child exit while exclusive recovery is still running.
	pidPath := p.PIDFile()
	pidRecord, err := currentDaemonPIDRecord(processStartTime, func() time.Time { return time.Now().UTC() })
	if err != nil {
		return fmt.Errorf("build pid file: %w", err)
	}
	if err := writeDaemonPIDFile(pidPath, pidRecord); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	defer func() {
		if pidData, err := os.ReadFile(pidPath); err == nil {
			if current, readErr := readDaemonPIDFileData(pidData); readErr == nil && current.PID == pidRecord.PID && current.StartedAt.Equal(pidRecord.StartedAt) {
				_ = os.Remove(pidPath)
			}
		}
	}()
	slog.Info("daemon process launched", "pid", pidRecord.PID)

	// Recovery remains exclusive and completes before IPC is bound.
	recoverOnStartup(d, p, mgr)

	srv := ipc.NewServer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var shutdownOnce sync.Once
	doShutdown := func(reason string) {
		shutdownOnce.Do(func() {
			slog.Info("shutting down", "reason", reason)
			mgr.Shutdown()
			cancel()
			srv.Close()
		})
	}

	registerHandlers(srv, mgr, d, func() { doShutdown("ipc request") })

	// Handle OS signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, daemonSignals()...)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case sig := <-sigCh:
			doShutdown(sig.String())
		case <-ctx.Done():
		}
	}()

	socketPath := p.Socket()
	bindStarted := time.Now()
	if err := srv.Listen(socketPath); err != nil {
		return fmt.Errorf("bind IPC: %w", err)
	}
	logStartupPhase("ipc_bind", bindStarted)

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- srv.ServeReady() }()
	healthStarted := time.Now()
	if err := confirmLocalIPCHealth(p, 2*time.Second); err != nil {
		srv.Close()
		<-serveErrCh
		return fmt.Errorf("confirm IPC health: %w", err)
	}
	logStartupPhase("ipc_health", healthStarted)
	slog.Info("daemon ready", "socket", socketPath, "pid", os.Getpid(), "startup_ms", time.Since(startupStarted).Milliseconds())

	if err := <-serveErrCh; err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	doShutdown("listener closed")

	// Clean up socket file only if we still own the PID file.
	// A new daemon may have already replaced the socket.
	if pidData, err := os.ReadFile(pidPath); err == nil {
		if current, readErr := readDaemonPIDFileData(pidData); readErr == nil && current.PID == pidRecord.PID && current.StartedAt.Equal(pidRecord.StartedAt) {
			os.Remove(pidPath)
			os.Remove(socketPath)
		}
	}
	slog.Info("daemon stopped")
	return nil
}

func confirmLocalIPCHealth(p *paths.Paths, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		alive, err := daemonIsRunningViaIPC(p)
		if err == nil && alive {
			return nil
		}
		if err != nil {
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("health did not report ready within %v", timeout)
}

func currentDaemonPIDRecord(startTime func(int) (time.Time, error), now func() time.Time) (daemonPIDFile, error) {
	pid := os.Getpid()
	startedAt, err := startTime(pid)
	if err != nil {
		startedAt = agent.CurrentProcessStartedAt()
		if startedAt.IsZero() {
			startedAt = now()
		}
	}
	return daemonPIDFile{PID: pid, StartedAt: startedAt.UTC()}, nil
}

func writeDaemonPIDFile(path string, record daemonPIDFile) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal pid file: %w", err)
	}
	tmp, err := createDaemonPIDTempFile(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create pid temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod pid temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write pid temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close pid temp file: %w", err)
	}
	if err := renameDaemonPIDFile(tmpPath, path); err != nil {
		return fmt.Errorf("rename pid file: %w", err)
	}
	tmpPath = ""
	return nil
}

// recoverOnStartup cleans up after a previous daemon crash by marking stale
// runs/steps as failed, killing orphaned managed-server subprocesses
// (opencode, rovodev), and removing orphaned worktree directories. It also
// best-effort migrates gate bare repos in place so older installs pick up
// the per-worktree hookspath isolation introduced for issue #122 when Git
// supports config --worktree.
func recoverOnStartup(d *db.DB, p *paths.Paths, mgr *RunManager) {
	orphanStarted := time.Now()
	reapOrphanedServers(p)
	logStartupPhase("orphan_servers", orphanStarted)

	gateStarted := time.Now()
	gateStats := migrateGateConfigs(context.Background(), d, p)
	logStartupPhase("gate_migration", gateStarted,
		"gate_count", gateStats.Gates,
		"current", gateStats.Current,
		"migrated", gateStats.Migrated,
		"rejected", gateStats.Rejected,
		"failed", gateStats.Failed,
	)

	terminalPRStarted := time.Now()
	terminalPRCount, err := d.ReconcileTerminalPRRuns()
	if err != nil {
		slog.Error("failed to reconcile terminal PR runs", "error", err)
		logStartupPhase("terminal_pr_runs", terminalPRStarted, "failed", true)
	} else {
		if terminalPRCount > 0 {
			slog.Info("reconciled terminal PR runs", "count", terminalPRCount)
		}
		logStartupPhase("terminal_pr_runs", terminalPRStarted, "reconciled", terminalPRCount)
	}

	parkedStarted := time.Now()
	plans := mgr.recoverableParkedRuns(context.Background())
	preserved := make(map[string]struct{}, len(plans))
	for _, plan := range plans {
		preserved[plan.run.ID] = struct{}{}
	}
	logStartupPhase("parked_runs", parkedStarted, "preserved", len(plans))

	staleStarted := time.Now()
	count, err := d.RecoverStaleRunsExcept("daemon crashed during execution", preserved)
	if err != nil {
		slog.Error("failed to recover stale runs", "error", err)
		logStartupPhase("stale_runs", staleStarted, "failed", true)
		for _, plan := range plans {
			_ = plan.agent.Close()
		}
		return
	}
	if count > 0 {
		slog.Info("recovered stale runs from previous crash", "count", count)
	}
	logStartupPhase("stale_runs", staleStarted, "recovered", count)

	worktreeStarted := time.Now()
	cleanupOrphanWorktrees(d, p)
	logStartupPhase("worktree_cleanup", worktreeStarted)
	mgr.resumeRecoveredRuns(plans)
}

// cleanupOrphanWorktrees removes worktree directories left behind by runs
// that are no longer active. It is DB-aware: a worktree is only removed when
// its run row is terminal, or when there is no matching run row at all.
// This is what keeps cleanup from deleting the checkout out from under a
// pipeline that is still actually running (see skipWorktreeCleanup).
// Called from recoverOnStartup after
// RecoverStaleRuns, so in the normal single-daemon path every run this loop
// sees has already been resolved to a terminal status; it is factored out
// separately so it can also be exercised - and its DB-aware skip behavior
// verified - independent of stale-run recovery's side effects.
func cleanupOrphanWorktrees(d *db.DB, p *paths.Paths) {
	wtRoot := p.WorktreesDir()
	entries, err := os.ReadDir(wtRoot)
	if err != nil {
		return // directory may not exist yet
	}
	ctx := context.Background()
	for _, repoEntry := range entries {
		if !repoEntry.IsDir() {
			continue
		}
		repoPath := filepath.Join(wtRoot, repoEntry.Name())
		gateDir := p.RepoDir(repoEntry.Name())
		runEntries, err := os.ReadDir(repoPath)
		if err != nil {
			continue
		}
		for _, runEntry := range runEntries {
			if !runEntry.IsDir() {
				continue
			}
			runID := runEntry.Name()
			wtPath := filepath.Join(repoPath, runID)
			if skip, reason := skipWorktreeCleanup(d, runID); skip {
				slog.Info("skipping worktree cleanup", "path", wtPath, "reason", reason)
				continue
			}
			if err := git.WorktreeRemove(ctx, gateDir, wtPath); err != nil {
				slog.Warn("git worktree remove failed, falling back to os.RemoveAll", "path", wtPath, "error", err)
				if err := os.RemoveAll(wtPath); err != nil {
					slog.Warn("failed to remove orphaned worktree", "path", wtPath, "error", err)
				}
			} else {
				slog.Info("removed orphaned worktree", "path", wtPath)
			}
		}
		// Remove empty repo dir.
		os.Remove(repoPath)
	}
}

// skipWorktreeCleanup reports whether the worktree directory for runID must
// be left alone during startup cleanup. It is the active-run guard that
// makes cleanup safe even if the singleton lock were ever bypassed: a
// worktree is never removed while its run is still pending or running -
// only terminal-run leftovers or directories with no matching run row at
// all (e.g. a directory left behind after its run row was independently
// pruned) are eligible for removal. RunManager.startRun always inserts the
// run row before creating the worktree directory, so on a single daemon a
// "no matching run" directory is never one whose insert simply hasn't landed
// yet - it is safe to remove immediately.
func skipWorktreeCleanup(d *db.DB, runID string) (bool, string) {
	run, err := d.GetRun(runID)
	if err != nil {
		return true, fmt.Sprintf("failed to look up run %s: %v", runID, err)
	}
	if run != nil && (run.Status == types.RunPending || run.Status == types.RunRunning) {
		return true, fmt.Sprintf("run %s is %s", runID, run.Status)
	}
	return false, ""
}

type gateMigrationStats struct {
	Gates    int
	Current  int
	Migrated int
	Rejected int
	Failed   int
}

var ensureGateHooksPathIsolation = git.EnsureHooksPathIsolation

// migrateGateConfigs discovers gates from authoritative DB records plus legacy
// directories with the strict <id>.git shape. Every unstamped candidate is
// structurally checked and explicitly verified as bare before any hook or Git
// mutation. A completed, content-versioned stamp makes normal restarts a cheap
// filesystem-only pass instead of six Git subprocesses per gate.
func migrateGateConfigs(ctx context.Context, d *db.DB, p *paths.Paths) gateMigrationStats {
	var stats gateMigrationStats
	candidates := make(map[string]struct{})
	reposDir := filepath.Clean(p.ReposDir())

	repos, err := d.GetRepos()
	if err != nil {
		slog.Warn("list authoritative gates for migration failed", "error", err)
		stats.Failed++
	} else {
		for _, repo := range repos {
			bareDir := filepath.Clean(p.RepoDir(repo.ID))
			if filepath.Dir(bareDir) != reposDir || filepath.Base(bareDir) != repo.ID+".git" {
				stats.Rejected++
				slog.Warn("rejecting unsafe authoritative gate path", "repo_id", repo.ID)
				continue
			}
			candidates[bareDir] = struct{}{}
		}
	}

	entries, readErr := os.ReadDir(reposDir)
	if readErr != nil && !os.IsNotExist(readErr) {
		slog.Warn("scan gate directory for migration failed", "error", readErr)
		stats.Failed++
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		id := strings.TrimSuffix(name, ".git")
		if id == name || id == "" || filepath.Base(name) != name {
			stats.Rejected++
			continue
		}
		candidates[filepath.Join(reposDir, name)] = struct{}{}
	}

	dirs := make([]string, 0, len(candidates))
	for bareDir := range candidates {
		dirs = append(dirs, bareDir)
	}
	sort.Strings(dirs)
	for _, bareDir := range dirs {
		if !git.LooksLikeBareRepository(bareDir) {
			stats.Rejected++
			slog.Warn("rejecting invalid gate directory", "bare", bareDir)
			continue
		}
		if git.GateConfigCurrent(bareDir) {
			stats.Gates++
			stats.Current++
			continue
		}
		if err := git.ValidateBareRepository(ctx, bareDir); err != nil {
			stats.Rejected++
			slog.Warn("rejecting non-bare gate directory", "bare", bareDir, "error", err)
			continue
		}
		stats.Gates++
		if err := migrateGateConfig(ctx, bareDir); err != nil {
			stats.Failed++
			slog.Warn("migrate gate config failed", "bare", bareDir, "error", err)
			continue
		}
		stats.Migrated++
	}
	return stats
}

func migrateGateConfig(ctx context.Context, bareDir string) error {
	if err := git.RefreshManagedGateHooks(bareDir); err != nil {
		return fmt.Errorf("refresh managed receive hooks: %w", err)
	}
	if _, err := git.RunBare(ctx, bareDir, "config", "receive.advertisePushOptions", "true"); err != nil {
		return fmt.Errorf("enable push options: %w", err)
	}
	isolated, err := ensureGateHooksPathIsolation(ctx, bareDir)
	if err != nil {
		return fmt.Errorf("isolate hooks path: %w", err)
	}
	if !isolated {
		return fmt.Errorf("isolate hooks path: git config --worktree is unsupported")
	}
	if err := git.MarkGateConfigCurrent(bareDir); err != nil {
		return fmt.Errorf("stamp gate config: %w", err)
	}
	return nil
}

func registerHandlers(srv *ipc.Server, mgr *RunManager, d *db.DB, shutdown func()) {
	classify := func(ctx context.Context, cwd string, markerPresent, skipManagedGit bool) (gatecontext.Result, error) {
		return (gatecontext.Inspector{DB: d, Paths: mgr.paths}).Inspect(ctx, gatecontext.Request{
			CWD:            cwd,
			PeerPID:        ipc.PeerPID(ctx),
			DaemonPID:      os.Getpid(),
			MarkerPresent:  markerPresent,
			SkipManagedGit: skipManagedGit,
		})
	}
	refuseNested := func(ctx context.Context, skipManagedGit bool) error {
		result, err := classify(ctx, "", false, skipManagedGit)
		if err != nil {
			return err
		}
		if result.Nested {
			return fmt.Errorf("%s", gatecontext.RefusalMessage(result))
		}
		return nil
	}

	srv.Handle(ipc.MethodHealth, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return &ipc.HealthResult{Status: "ok"}, nil
	})

	srv.Handle(ipc.MethodShutdown, func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
		if err := refuseNested(ctx, false); err != nil {
			return nil, err
		}
		go shutdown()
		return &ipc.ShutdownResult{OK: true}, nil
	})

	srv.Handle(ipc.MethodGetRun, func(_ context.Context, params json.RawMessage) (interface{}, error) {
		var p ipc.GetRunParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		run, err := d.GetRun(p.RunID)
		if err != nil {
			return nil, fmt.Errorf("get run: %w", err)
		}
		if run == nil {
			return nil, fmt.Errorf("run not found: %s", p.RunID)
		}
		steps, err := d.GetStepsByRun(p.RunID)
		if err != nil {
			return nil, fmt.Errorf("get steps: %w", err)
		}
		return &ipc.GetRunResult{Run: runToInfo(d, run, steps)}, nil
	})

	srv.Handle(ipc.MethodGetRuns, func(_ context.Context, params json.RawMessage) (interface{}, error) {
		var p ipc.GetRunsParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		runs, err := d.GetRunsByRepo(p.RepoID)
		if err != nil {
			return nil, fmt.Errorf("get runs: %w", err)
		}
		infos := make([]ipc.RunInfo, 0, len(runs))
		for _, r := range runs {
			steps, err := d.GetStepsByRun(r.ID)
			if err != nil {
				return nil, fmt.Errorf("get steps for run %s: %w", r.ID, err)
			}
			infos = append(infos, *runToInfo(d, r, steps))
		}
		return &ipc.GetRunsResult{Runs: infos}, nil
	})

	srv.Handle(ipc.MethodGetRunsForHead, func(_ context.Context, params json.RawMessage) (interface{}, error) {
		var p ipc.GetRunsForHeadParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		runs, err := d.GetRunsByRepoHead(p.RepoID, p.Branch, p.HeadSHA)
		if err != nil {
			return nil, fmt.Errorf("get runs for head: %w", err)
		}
		infos := make([]ipc.RunInfo, 0, len(runs))
		for _, r := range runs {
			steps, err := d.GetStepsByRun(r.ID)
			if err != nil {
				return nil, fmt.Errorf("get steps for run %s: %w", r.ID, err)
			}
			infos = append(infos, *runToInfo(d, r, steps))
		}
		return &ipc.GetRunsResult{Runs: infos}, nil
	})

	srv.Handle(ipc.MethodGetActiveRun, func(_ context.Context, params json.RawMessage) (interface{}, error) {
		var p ipc.GetActiveRunParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		run, err := d.GetActiveRun(p.RepoID, p.Branch)
		if err != nil {
			return nil, fmt.Errorf("get active run: %w", err)
		}
		if run == nil {
			return &ipc.GetActiveRunResult{}, nil
		}
		steps, err := d.GetStepsByRun(run.ID)
		if err != nil {
			return nil, fmt.Errorf("get steps: %w", err)
		}
		return &ipc.GetActiveRunResult{Run: runToInfo(d, run, steps)}, nil
	})

	srv.Handle(ipc.MethodGateContext, func(ctx context.Context, params json.RawMessage) (interface{}, error) {
		var p ipc.GateContextParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		result, err := classify(ctx, p.CWD, p.MarkerPresent, false)
		if err != nil {
			return nil, err
		}
		return gateContextResult(result), nil
	})

	srv.Handle(ipc.MethodAdmitPush, func(ctx context.Context, params json.RawMessage) (interface{}, error) {
		var p ipc.AdmitPushParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if strings.TrimSpace(p.Gate) == "" {
			return nil, fmt.Errorf("gate path is required")
		}
		result, err := classify(ctx, "", false, true)
		if err != nil {
			return nil, err
		}
		return &ipc.AdmitPushResult{Context: gateContextResult(result)}, nil
	})

	srv.Handle(ipc.MethodRerun, func(ctx context.Context, params json.RawMessage) (interface{}, error) {
		if err := refuseNested(ctx, false); err != nil {
			return nil, err
		}
		var p ipc.RerunParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		runID, err := mgr.HandleRerun(ctx, p.RepoID, p.Branch, p.SkipSteps, p.Intent)
		if err != nil {
			return nil, err
		}
		return &ipc.RerunResult{RunID: runID}, nil
	})

	srv.Handle(ipc.MethodPushReceived, func(ctx context.Context, params json.RawMessage) (interface{}, error) {
		// Hooks execute in a managed bare gate by definition, so only the
		// authenticated peer ancestry is meaningful at this ingress.
		if err := refuseNested(ctx, true); err != nil {
			return nil, err
		}
		var p ipc.PushReceivedParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		slog.Info("push received", "ref", p.Ref, "old", p.Old, "new", p.New, "gate", p.Gate)
		runID, err := mgr.HandlePushReceived(ctx, &p)
		if err != nil {
			return nil, err
		}
		return &ipc.PushReceivedResult{RunID: runID}, nil
	})

	srv.Handle(ipc.MethodRespond, func(ctx context.Context, params json.RawMessage) (interface{}, error) {
		if err := refuseNested(ctx, false); err != nil {
			return nil, err
		}
		var p ipc.RespondParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if err := mgr.HandleRespondWithOverrides(p.RunID, p.Step, p.Action, p.FindingIDs, p.Instructions, p.AddedFindings); err != nil {
			return nil, err
		}
		return &ipc.RespondResult{OK: true}, nil
	})

	srv.Handle(ipc.MethodCancelRun, func(ctx context.Context, params json.RawMessage) (interface{}, error) {
		if err := refuseNested(ctx, false); err != nil {
			return nil, err
		}
		var p ipc.CancelRunParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if err := mgr.HandleCancel(p.RunID); err != nil {
			return nil, err
		}
		return &ipc.CancelRunResult{OK: true}, nil
	})

	srv.HandleStream(ipc.MethodSubscribe, func(ctx context.Context, params json.RawMessage) (ipc.StreamFunc, error) {
		var p ipc.SubscribeParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}

		// Register before returning the prepared stream. The IPC server sends
		// its acknowledgement only after this point, so a client's immediate
		// full reconciliation cannot race an unregistered subscription.
		ch, unsub := mgr.Subscribe(p.RunID)
		var unsubscribeOnce sync.Once
		cleanup := func() { unsubscribeOnce.Do(unsub) }
		go func() {
			<-ctx.Done()
			cleanup()
		}()
		return func(send func(interface{}) error) error {
			defer cleanup()
			for {
				select {
				case event, ok := <-ch:
					if !ok {
						return nil // channel closed (run completed)
					}
					if err := send(event); err != nil {
						return err // client disconnected
					}
				case <-ctx.Done():
					return nil
				}
			}
		}, nil
	})
}

func gateContextResult(result gatecontext.Result) ipc.GateContextResult {
	return ipc.GateContextResult{
		Nested:           result.Nested,
		ManagedGit:       result.ManagedGit,
		AgentDescendant:  result.AgentDescendant,
		DaemonDescendant: result.DaemonDescendant,
		MarkerPresent:    result.MarkerPresent,
		RunID:            result.RunID,
		Phase:            result.Phase,
	}
}

func runToInfo(d *db.DB, r *db.Run, steps []*db.StepResult) *ipc.RunInfo {
	info := &ipc.RunInfo{
		ID:                 r.ID,
		RepoID:             r.RepoID,
		Branch:             r.Branch,
		HeadSHA:            r.HeadSHA,
		SubmittedHeadSHA:   r.SubmittedHeadSHA,
		BaseSHA:            r.BaseSHA,
		Status:             r.Status,
		PRURL:              r.PRURL,
		Error:              r.Error,
		CIReady:            r.CIReadyAt != nil,
		AwaitingAgent:      r.AwaitingAgentSince != nil,
		AwaitingAgentSince: r.AwaitingAgentSince,
		CreatedAt:          r.CreatedAt,
		UpdatedAt:          r.UpdatedAt,
	}
	if len(steps) > 0 {
		info.Steps = make([]ipc.StepResultInfo, 0, len(steps))
		for _, s := range steps {
			info.Steps = append(info.Steps, stepToInfo(d, s))
		}
	}
	return info
}

func stepToInfo(d *db.DB, s *db.StepResult) ipc.StepResultInfo {
	info := ipc.StepResultInfo{
		ID:             s.ID,
		RunID:          s.RunID,
		StepName:       s.StepName,
		StepOrder:      s.StepOrder,
		Status:         s.Status,
		ExitCode:       s.ExitCode,
		DurationMS:     s.DurationMS,
		FindingsJSON:   s.FindingsJSON,
		Error:          s.Error,
		StartedAt:      s.StartedAt,
		CompletedAt:    s.CompletedAt,
		LastActivityAt: s.LastActivityAt,
		LastActivity:   s.LastActivity,
		AgentPID:       s.AgentPID,
	}
	if s.AutoFixLimit != nil {
		info.AutoFixLimit = *s.AutoFixLimit
	}
	if stats, err := d.StepFindingStats(s); err == nil {
		info.ReportedFindings = stats.ReportedFindings
		info.FixedFindings = stats.FixedFindings
	}
	if summaries, err := d.StepFixSummaries(s.ID); err == nil {
		info.FixSummaries = summaries
	}
	if rounds, err := d.StepRoundStats(s.ID); err == nil {
		info.RoundCount = rounds.TotalRounds
		info.FixRoundCount = rounds.FixRounds
		info.PendingFixSource = rounds.PendingFixSource
	}
	return info
}
