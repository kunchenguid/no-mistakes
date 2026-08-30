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
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/logstore"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/procreap"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

// orphanProcessMinAge is the age floor for the startup orphan-process sweep.
// Startup is the one moment where the daemon has no way to tell a leaked
// process from one belonging to a run that is starting concurrently, so
// anything young is left alone; run cleanup sweeps its own worktree with no
// age floor because it owns that run.
var orphanProcessMinAge = procreap.DefaultMinAge

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
	resolvedCfg := config.Merge(globalCfg, &config.RepoConfig{})
	if err := p.ValidateEvidenceRoot(resolvedCfg.Test.Evidence.LocalRoot); err != nil {
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

	return runWithOptionsLocked(p, d, globalCfg, nil, startupStarted)
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

	globalCfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	return runWithOptionsLocked(p, d, globalCfg, stepFactory, startupStarted)
}

// runWithOptionsLocked takes the global configuration its caller already loaded,
// so startup reads and validates config.yaml exactly once. Re-reading it per
// consumer would let one startup act on two different documents, and every later
// read needs a fallback for a failure the caller has already refused to start on.
func runWithOptionsLocked(p *paths.Paths, d *db.DB, globalCfg *config.GlobalConfig, stepFactory StepFactory, startupStarted time.Time) error {
	// Refuse an unusable worktree placement before anything walks, sweeps, or
	// removes a directory under it. This is the second half of worktree_roots
	// validation: internal/config checks every entry it can judge without
	// knowing NM_HOME, and this process is the one that knows.
	layout, err := validatedWorktreeLayout(d, p, globalCfg)
	if err != nil {
		return err
	}

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
	recoverOnStartup(d, p, mgr, layout)

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
func recoverOnStartup(d *db.DB, p *paths.Paths, mgr *RunManager, layout *worktrees.Layout) {
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

	// Read while the runs that were executing when this daemon started still say
	// so: recovery below turns them terminal, and they are the ones whose
	// worktree may already be gone with something still standing in it.
	activeWorktrees := activeRecordedRunWorktrees(d, p)
	preserveStaleRunHeads(d, p, preserved)

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

	reportUnusableWorktreeRoots(d, layout)
	leftover := leftoverRecordedRunWorktrees(d, p)

	orphanProcStarted := time.Now()
	sweepOrphanRunProcesses(d, p, sweepableWorktrees(leftover, activeWorktrees))
	logStartupPhase("orphan_processes", orphanProcStarted)

	worktreeStarted := time.Now()
	cleanupOrphanWorktrees(d, p, leftover)
	logStartupPhase("worktree_cleanup", worktreeStarted)

	// Evidence is reaped after stale-run recovery for the same reason worktrees
	// are: every run's status is settled by now, so the active-run guard can
	// tell a crashed run's leftovers from work still in flight.
	evidenceStarted := time.Now()
	global, cfgErr := config.LoadGlobal(p.ConfigFile())
	if cfgErr != nil {
		slog.Warn("failed to load global config for evidence reaping, using defaults", "error", cfgErr)
		global = nil
	}
	policy := evidenceReapPolicyFor(global)
	root := evidenceRootFor(p, global)
	now := time.Now()
	reapEvidence(d, root, policy, now)
	reapLegacyEvidence(d, root, policy, now)
	logStartupPhase("evidence_cleanup", evidenceStarted)

	mgr.resumeRecoveredRuns(plans)
}

func preserveStaleRunHeads(d *db.DB, p *paths.Paths, excluded map[string]struct{}) {
	active, err := d.ActiveRunWorktrees()
	if err != nil {
		slog.Warn("failed to list active run heads before crash recovery", "error", err)
		return
	}
	for _, wt := range active {
		if _, skip := excluded[wt.RunID]; skip {
			continue
		}
		run, err := d.GetRun(wt.RunID)
		if err != nil || run == nil {
			continue
		}
		workDir := worktrees.RecordedDir(p, wt.Dir, wt.RepoID, wt.RunID)
		if _, err := os.Stat(workDir); err != nil {
			continue
		}
		preserveRunHead(d, workDir, run)
	}
}

func preserveRunHead(d *db.DB, workDir string, run *db.Run) (string, bool) {
	if d == nil || run == nil || strings.TrimSpace(workDir) == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	head, err := git.HeadSHA(ctx, workDir)
	if err != nil {
		slog.Warn("failed to read managed worktree head before terminalization", "run", run.ID, "error", err)
		return "", false
	}
	recorded := strings.TrimSpace(run.HeadSHA)
	if recorded == "" || head != recorded && !isGitAncestor(ctx, workDir, recorded, head) {
		slog.Warn("managed worktree head is not a verified descendant before terminalization", "run", run.ID, "recorded", recorded, "observed", head)
		return "", false
	}
	published := ""
	if run.LastPushedSHA != nil {
		published = *run.LastPushedSHA
	} else if run.SubmittedHeadSHA != nil {
		published = *run.SubmittedHeadSHA
	}
	if head != published {
		if err := custody.PreserveRecoveryHead(ctx, workDir, run.ID, head); err != nil {
			slog.Warn("failed to anchor managed worktree head before terminalization", "run", run.ID, "head", head, "error", err)
			return "", false
		}
	}
	if err := d.RecordRunTerminalHeadEvidence(run.ID, head); err != nil {
		slog.Warn("failed to record managed worktree head before terminalization", "run", run.ID, "head", head, "error", err)
		return "", false
	}
	return head, true
}

func isGitAncestor(ctx context.Context, dir, ancestor, descendant string) bool {
	_, err := git.Run(ctx, dir, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// sweepOrphanRunProcesses terminates processes still standing in a run
// worktree that no run owns any more. A predecessor daemon's group teardown
// cannot reach a child that left its process group (see internal/procreap),
// and once that child reparents to init nothing lineage-based can name it
// again - it just keeps burning CPU and holding a deleted worktree open. This
// runs after stale-run recovery so every run's status is settled, and before
// worktree cleanup so the directories are freed of their holders first.
//
// Reach outside the default worktrees tree is exactly the recorded run
// worktrees, never a configured root: signalling in an operator's own directory
// is limited to the directories our own run rows name, matching what cleanup
// there may remove, and a placement the operator has since reconfigured away is
// still swept because the run recorded it.
func sweepOrphanRunProcesses(d *db.DB, p *paths.Paths, worktrees []procreap.Worktree) {
	ctx := context.Background()
	wtRoot := p.WorktreesDir()
	pathByRun := make(map[string]string, len(worktrees))
	for _, wt := range worktrees {
		pathByRun[wt.RunID] = wt.Dir
	}
	procreap.SweepAndLog(procreap.Options{
		WorktreesRoot: wtRoot,
		Worktrees:     worktrees,
		MinAge:        orphanProcessMinAge,
		RunActive: func(repoID, runID string) bool {
			wtPath := pathByRun[runID]
			if wtPath == "" {
				wtPath = filepath.Join(wtRoot, repoID, runID)
			}
			skip, _ := skipWorktreeCleanup(ctx, d, runID, wtPath)
			return skip
		},
	}, "daemon_startup")
}

// sweepableWorktrees is the procreap view of the run worktrees outside the
// default tree, and the bound on how many of them the sweep carries.
//
// Every entry becomes a path matcher tested against every candidate process, so
// the cost is per entry, not per directory that exists - and run rows are never
// pruned, so the full recorded history would make each restart of a long-lived
// install slower than the last. Two sets are enough to reach every process the
// sweep can legitimately reach, and both are bounded by the present rather than
// by history:
//
//   - leftover: a recorded worktree still on disk. Anything standing in one is
//     reachable through it, and cleanup is about to try to remove it anyway.
//   - active: the recorded worktree of a run that was executing when this
//     daemon started, whose directory may already be gone while a process that
//     escaped its process group still holds it as its cwd. A run that reached a
//     terminal state has had that sweep run against its own worktree already.
func sweepableWorktrees(sets ...[]db.RunWorktree) []procreap.Worktree {
	var out []procreap.Worktree
	seen := make(map[string]bool)
	for _, set := range sets {
		for _, wt := range set {
			if seen[wt.Dir] {
				continue
			}
			seen[wt.Dir] = true
			out = append(out, procreap.Worktree{Dir: wt.Dir, RepoID: wt.RepoID, RunID: wt.RunID})
		}
	}
	return out
}

// leftoverRecordedRunWorktrees returns the run worktrees this machine placed
// outside <NM_HOME>/worktrees that are still on disk, as the runs that created
// them recorded them (see internal/worktrees). It is what startup cleanup
// removes there, so cleanup depends on no configuration at all: an operator who
// edits or drops a worktree_roots entry after a crash would otherwise hide a
// directory a run is still recorded as owning, and nothing would ever name it
// again.
//
// Only rows whose directory is still there survive, so everything DOWNSTREAM -
// the canonical-path comparison, the process sweep, the removals - is
// proportional to what is actually left over, which is normally nothing.
//
// The filter itself is not: run rows are never pruned, so this pays one os.Stat
// per run this machine has ever placed outside the default tree, on every
// startup, before the socket is bound. That cost is accepted rather than bounded.
// A cheaper set cannot be had from the run status here, because this runs AFTER
// crash recovery has already turned the runs that left directories behind
// terminal - the status that would filter them out is the status that identifies
// them. Bounding it by age instead would trade a stat-per-row for a directory
// nobody ever removes, which is the failure this exists to prevent.
func leftoverRecordedRunWorktrees(d *db.DB, p *paths.Paths) []db.RunWorktree {
	recorded, err := d.RunWorktreesOutside(p.WorktreesDir())
	if err != nil {
		slog.Warn("failed to list recorded run worktrees; skipping placements outside the default worktrees directory", "error", err)
		return nil
	}
	return outsideDefaultWorktreesTree(p, onDisk(recorded))
}

// activeRecordedRunWorktrees returns the recorded placements of the runs that
// are still active, whose directories are deliberately NOT required to exist
// (see db.ActiveRunWorktreesOutside). Call it before crash recovery settles run
// statuses.
func activeRecordedRunWorktrees(d *db.DB, p *paths.Paths) []db.RunWorktree {
	recorded, err := d.ActiveRunWorktreesOutside(p.WorktreesDir())
	if err != nil {
		slog.Warn("failed to list active run worktrees; skipping placements outside the default worktrees directory", "error", err)
		return nil
	}
	return outsideDefaultWorktreesTree(p, recorded)
}

// onDisk keeps the recorded placements that still name a directory.
func onDisk(recorded []db.RunWorktree) []db.RunWorktree {
	out := make([]db.RunWorktree, 0, len(recorded))
	for _, wt := range recorded {
		if info, err := os.Stat(wt.Dir); err != nil || !info.IsDir() {
			continue
		}
		out = append(out, wt)
	}
	return out
}

// outsideDefaultWorktreesTree drops placements that are in the default tree
// after all, which the query's byte-prefix test cannot decide for a second
// spelling of the same directory (/var and /private/var). The default tree is
// discovered by walking it, so a row that belongs to it must not be handled
// twice.
func outsideDefaultWorktreesTree(p *paths.Paths, recorded []db.RunWorktree) []db.RunWorktree {
	worktreesDir := p.WorktreesDir()
	out := make([]db.RunWorktree, 0, len(recorded))
	for _, wt := range recorded {
		if worktrees.Contains(worktreesDir, wt.Dir) {
			continue
		}
		out = append(out, wt)
	}
	return out
}

// validatedWorktreeLayout is the one worktree placement startup resolves, and it
// fails startup when that placement is one this machine cannot host (see
// worktrees.CheckPlacement for which placements those are and why). Refusing to
// start is the same treatment an unreadable global config already gets, and
// for the same reason: the daemon's first act is crash recovery, which walks
// and removes directories, so a placement it would misread must be rejected
// before that runs rather than warned about afterwards.
//
// Because the layout it returns is the validated one, every later startup
// consumer is handed it rather than rebuilding one from another read of the same
// file - which would have to carry a fallback for a failure this function has
// already refused to start on.
//
// This is where the registered repositories are known, so it is the only layer
// that can judge a root placed inside a checkout other than the one whose runs
// it holds. A repository registered after such a root was configured is caught
// here on the next startup.
func validatedWorktreeLayout(d *db.DB, p *paths.Paths, globalCfg *config.GlobalConfig) (*worktrees.Layout, error) {
	layout := worktrees.New(p, globalCfg.WorktreeRoots)
	checkouts, err := registeredCheckouts(d)
	if err != nil {
		return nil, fmt.Errorf("list registered checkouts for worktree placement: %w", err)
	}
	if err := layout.Validate(checkouts...); err != nil {
		return nil, fmt.Errorf("configured worktree placement is unusable: %w", err)
	}
	return layout, nil
}

// registeredCheckouts lists the working paths of every registered repository, so
// placement validation can name a checkout a configured root would dirty. A
// failure to read them is fatal to the caller's guard: an unreadable repository
// list means the guard cannot see the set it protects, and a guard that cannot
// see its set must refuse rather than silently validate against nothing — the
// database has already opened and migrated by the time any caller runs, so the
// failure is never routine.
func registeredCheckouts(d *db.DB) ([]string, error) {
	if d == nil {
		return nil, nil
	}
	return d.RepoWorkingPaths()
}

// reportUnusableWorktreeRoots names configured placements that will not do
// what the operator expects. A worktree_roots key is matched against a
// registered checkout path, so a stale key left behind by a moved or ejected
// checkout - or one that differs from the recorded path in a way this
// filesystem does not consider equal, such as letter case - silently places
// nothing, which has no other symptom than runs continuing to appear under
// NM_HOME. That is the one placement worth only a log line: a root that does
// not work at all already refused startup (see
// validatedWorktreeLayout and worktrees.Layout.Validate).
func reportUnusableWorktreeRoots(d *db.DB, layout *worktrees.Layout) {
	checkouts := layout.Checkouts()
	if len(checkouts) == 0 {
		return
	}
	repos, err := d.GetRepos()
	if err != nil {
		slog.Warn("failed to list repositories while checking configured worktree roots", "error", err)
		return
	}
	registered := make(map[string]bool, len(repos))
	for _, repo := range repos {
		registered[worktrees.Canonical(repo.WorkingPath)] = true
	}
	for _, checkout := range checkouts {
		if !registered[checkout] {
			slog.Warn("worktree_roots entry matches no registered repository; its runs use the default placement", "checkout", checkout)
		}
	}
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
// verified - independent of stale-run recovery's side effects. Worktrees the
// operator placed outside this tree are named by recordedOrphanWorktrees, which
// never walks a directory it does not own.
//
// Every directory it is going to remove is swept in ONE process snapshot before
// any of them is removed. The sweep-before-removal invariant is what matters
// (see procreap.SweepRunWorktrees), not that each directory gets a snapshot of
// its own: reading the process table is the expensive part, a scoped sweep has
// no age floor so every process on the machine is a candidate, and this whole
// pass runs before the daemon binds its socket, against the startup budget. A
// crash that leaves several directories behind would otherwise make the daemon
// slower to start the more there is to clean up.
func cleanupOrphanWorktrees(d *db.DB, p *paths.Paths, leftover []db.RunWorktree) {
	ctx := context.Background()
	removable, repoDirs := defaultTreeOrphanWorktrees(d, p)
	removable = append(removable, recordedOrphanWorktrees(d, p, leftover)...)

	sweepable := make([]procreap.Worktree, 0, len(removable))
	for _, wt := range removable {
		sweepable = append(sweepable, procreap.Worktree{Dir: wt.dir, RepoID: wt.repoID, RunID: wt.runID})
	}
	sweepRunWorktrees(p.WorktreesDir(), sweepable, "worktree_cleanup")

	for _, wt := range removable {
		removeOrphanWorktree(ctx, wt)
	}
	for _, dir := range repoDirs {
		os.Remove(dir)
	}
}

// orphanWorktree is one run worktree directory startup cleanup has decided it
// may remove, resolved before anything is swept or removed.
type orphanWorktree struct {
	gateDir string
	dir     string
	repoID  string
	runID   string
}

// defaultTreeOrphanWorktrees walks <NM_HOME>/worktrees, which no-mistakes owns
// outright, and returns the run worktrees no run still owns plus the repository
// directories to drop once they are empty.
func defaultTreeOrphanWorktrees(d *db.DB, p *paths.Paths) (removable []orphanWorktree, repoDirs []string) {
	wtRoot := p.WorktreesDir()
	entries, err := os.ReadDir(wtRoot)
	if err != nil {
		return nil, nil // directory may not exist yet
	}
	for _, repoEntry := range entries {
		if !repoEntry.IsDir() {
			continue
		}
		repoPath := filepath.Join(wtRoot, repoEntry.Name())
		runEntries, err := os.ReadDir(repoPath)
		if err != nil {
			continue
		}
		for _, runEntry := range runEntries {
			if !runEntry.IsDir() {
				continue
			}
			wt := orphanWorktree{
				gateDir: p.RepoDir(repoEntry.Name()),
				dir:     filepath.Join(repoPath, runEntry.Name()),
				repoID:  repoEntry.Name(),
				runID:   runEntry.Name(),
			}
			if removableOrphanWorktree(d, wt) {
				removable = append(removable, wt)
			}
		}
		repoDirs = append(repoDirs, repoPath)
	}
	return removable, repoDirs
}

// recordedOrphanWorktrees names the leftover worktrees of runs the operator
// placed outside <NM_HOME>/worktrees (see worktree_roots in the global config).
//
// That directory belongs to the operator, not to no-mistakes: it holds the
// mise.local.toml, .envrc, or scratch checkout that motivated pointing runs at
// it in the first place, and it can hold a run-shaped directory this daemon
// never created - another tool's, or one left by a repository that used to
// point here. So unlike the default tree, which is discovered by walking, this
// names only the exact directories run records name, which is the same rule
// eject applies (see gate.removeRepoWorktrees): nothing there is enumerated,
// and a directory no run recorded is never even looked at. Whether a named
// directory may go is still the active-run guard (see skipWorktreeCleanup).
func recordedOrphanWorktrees(d *db.DB, p *paths.Paths, leftover []db.RunWorktree) []orphanWorktree {
	var removable []orphanWorktree
	for _, wt := range leftover {
		if info, err := os.Stat(wt.Dir); err != nil || !info.IsDir() {
			continue
		}
		candidate := orphanWorktree{gateDir: p.RepoDir(wt.RepoID), dir: wt.Dir, repoID: wt.RepoID, runID: wt.RunID}
		if removableOrphanWorktree(d, candidate) {
			removable = append(removable, candidate)
		}
	}
	return removable
}

// removableOrphanWorktree reports whether cleanup may take this directory,
// which is the active-run guard (see skipWorktreeCleanup). A directory it
// spares is neither swept nor removed.
func removableOrphanWorktree(d *db.DB, wt orphanWorktree) bool {
	if skip, reason := skipWorktreeCleanup(context.Background(), d, wt.runID, wt.dir); skip {
		slog.Info("skipping worktree cleanup", "path", wt.dir, "reason", reason)
		return false
	}
	return true
}

// removeOrphanWorktree removes one run worktree directory its caller has
// already decided on and swept (see cleanupOrphanWorktrees).
func removeOrphanWorktree(ctx context.Context, wt orphanWorktree) {
	gateDir, wtPath := wt.gateDir, wt.dir
	if err := git.WorktreeRemove(ctx, gateDir, wtPath); err != nil {
		slog.Warn("git worktree remove failed, falling back to os.RemoveAll", "path", wtPath, "error", err)
		if err := os.RemoveAll(wtPath); err != nil {
			slog.Warn("failed to remove orphaned worktree", "path", wtPath, "error", err)
		}
	} else {
		slog.Info("removed orphaned worktree", "path", wtPath)
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
//
// A run marked RunCIMonitorInterrupted (the daemon restarted while monitoring
// CI for an already-open PR, issue #361) is terminal and would otherwise leak
// its checkout on every future restart. Such a worktree is reclaimed like any
// other terminal-run leftover EXCEPT when it may hold unpushed work: a CI
// auto-fix commits locally before pushing (see steps/ci_fix.go), so a crash in
// that window leaves the only copy of the fix commit in this checkout. We
// reclaim only when the worktree HEAD equals the head the run already pushed -
// run.HeadSHA advances solely after a verified push, so a match proves nothing
// local is unpushed - and fail safe to preservation on any mismatch or
// unreadable HEAD so recoverable commits are never discarded.
func skipWorktreeCleanup(ctx context.Context, d *db.DB, runID, wtPath string) (bool, string) {
	run, err := d.GetRun(runID)
	if err != nil {
		return true, fmt.Sprintf("failed to look up run %s: %v", runID, err)
	}
	if run != nil && (run.Status == types.RunPending || run.Status == types.RunRunning) {
		return true, fmt.Sprintf("run %s is %s", runID, run.Status)
	}
	if run != nil && run.Status == types.RunCIMonitorInterrupted {
		head, err := git.HeadSHA(ctx, wtPath)
		if err != nil {
			return true, fmt.Sprintf("run %s ci monitor interrupted; worktree head unreadable (%v); preserving", runID, err)
		}
		if strings.TrimSpace(head) != run.HeadSHA {
			return true, fmt.Sprintf("run %s ci monitor interrupted; worktree may hold unpushed commits; preserving", runID)
		}
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

var sweepRunWorktrees = procreap.SweepRunWorktrees

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
		info, err := runSnapshot(mgr, p.RunID, func(runID string) (*ipc.RunInfo, error) {
			run, err := d.GetRun(runID)
			if err != nil {
				return nil, fmt.Errorf("get run: %w", err)
			}
			if run == nil {
				return nil, fmt.Errorf("run not found: %s", runID)
			}
			steps, err := d.GetStepsByRun(runID)
			if err != nil {
				return nil, fmt.Errorf("get steps: %w", err)
			}
			return runToInfo(d, run, steps), nil
		})
		if err != nil {
			return nil, err
		}
		return &ipc.GetRunResult{Run: info}, nil
	})

	// The fix-review diff is derived on demand instead of riding the event
	// stream, so a very large change can no longer produce an oversized frame
	// that takes the whole subscription down with it.
	srv.Handle(ipc.MethodGetStepDiff, func(ctx context.Context, params json.RawMessage) (interface{}, error) {
		var p ipc.GetStepDiffParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		diff, truncated, err := mgr.StepDiff(ctx, p.RunID)
		if err != nil {
			return nil, err
		}
		return &ipc.GetStepDiffResult{Diff: diff, Truncated: truncated}, nil
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
		runID, err := mgr.HandleRerun(ctx, p.RepoID, p.Branch, p.PreviousRunID, p.SkipSteps, p.Intent)
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
		sub, err := mgr.Subscribe(p.RunID)
		if err != nil {
			return nil, err
		}
		var unsubscribeOnce sync.Once
		cleanup := func() { unsubscribeOnce.Do(sub.Close) }
		go func() {
			<-ctx.Done()
			cleanup()
		}()
		return func(send func(interface{}) error) error {
			defer cleanup()
			for {
				event, ok := sub.Next(ctx)
				if !ok {
					return nil // stream finished (run completed or cancelled)
				}
				if err := send(event); err != nil {
					return err // client disconnected
				}
			}
		}, nil
	})
}

// runSnapshot reads an authoritative run snapshot and stamps it with the state
// revision sampled BEFORE the read.
//
// The ordering is the whole point and must not be reversed. Every producer
// writes state and only then broadcasts (see the executor's emitters), so a
// revision sampled first is never newer than the snapshot that follows it:
//
//   - every event at or below the sampled revision already has its write
//     reflected in the read, so nothing is lost by the consumer skipping it;
//   - every event above it is still delivered and still exceeds the snapshot's
//     revision, so the consumer still applies it on top.
//
// Sampling after the read would let a transition that landed in between be
// skipped by the consumer's monotonic guard and never repaired.
func runSnapshot(mgr *RunManager, runID string, read func(string) (*ipc.RunInfo, error)) (*ipc.RunInfo, error) {
	stateRev := mgr.StateRev(runID)
	info, err := read(runID)
	if err != nil {
		return nil, err
	}
	info.StateRev = stateRev
	return info, nil
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
		CIReadyNoCI:        r.CIReadyNoCI,
		AwaitingAgent:      r.AwaitingAgentSince != nil,
		AwaitingAgentSince: r.AwaitingAgentSince,
		CreatedAt:          r.CreatedAt,
		UpdatedAt:          r.UpdatedAt,
	}
	if len(steps) > 0 {
		info.Steps = make([]ipc.StepResultInfo, 0, len(steps))
		for _, s := range steps {
			stepInfo := stepToInfo(d, s)
			info.Steps = append(info.Steps, stepInfo)
			if info.CIOverrideReason == "" && stepInfo.OverrideReason != "" {
				info.CIOverrideReason = stepInfo.OverrideReason
			}
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
	if s.OverrideReason != nil {
		info.OverrideReason = *s.OverrideReason
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
