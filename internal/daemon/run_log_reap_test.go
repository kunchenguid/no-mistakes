package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// runLogFixture builds a DB plus a logs root and returns helpers for seeding
// per-run step-log directories with a chosen age and status.
type runLogFixture struct {
	t    *testing.T
	db   *db.DB
	p    *paths.Paths
	repo *db.Repo
	root string
}

func newRunLogFixture(t *testing.T) *runLogFixture {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	repo, err := d.InsertRepoWithID("repo1", "/nonexistent/work", "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}
	root := p.LogsDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return &runLogFixture{t: t, db: d, p: p, repo: repo, root: root}
}

// seed creates a run in the given status plus its step-log directory, aged by
// backdating the directory's modification time.
func (f *runLogFixture) seed(branch string, status types.RunStatus, age time.Duration) string {
	f.t.Helper()
	run, err := f.db.InsertRun(f.repo.ID, branch, "head-"+branch, "base-"+branch)
	if err != nil {
		f.t.Fatal(err)
	}
	if status != types.RunPending {
		if err := f.db.UpdateRunStatus(run.ID, status); err != nil {
			f.t.Fatal(err)
		}
	}
	dir := f.p.RunLogDir(run.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	logFile := filepath.Join(dir, "review.log")
	if err := os.WriteFile(logFile, []byte("output"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(dir, when, when); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chtimes(logFile, when, when); err != nil {
		f.t.Fatal(err)
	}
	return run.ID
}

func (f *runLogFixture) exists(runID string) bool {
	f.t.Helper()
	_, err := os.Stat(f.p.RunLogDir(runID))
	return err == nil
}

// TestReapRunLogsHonorsRetentionAndSparesActiveRuns mirrors the evidence
// reaper's core contract for the per-run step-log directory: a step may still
// be appending to a pending or running run's log, so it is never touched at
// any age.
func TestReapRunLogsHonorsRetentionAndSparesActiveRuns(t *testing.T) {
	f := newRunLogFixture(t)

	fresh := f.seed("fresh", types.RunCompleted, time.Hour)
	stale := f.seed("stale", types.RunCompleted, 30*24*time.Hour)
	activePending := f.seed("active-pending", types.RunPending, 30*24*time.Hour)
	activeRunning := f.seed("active-running", types.RunRunning, 30*24*time.Hour)

	reapRunLogs(f.db, f.root, evidenceReapPolicy{Retention: 14 * 24 * time.Hour}, time.Now())

	if !f.exists(fresh) {
		t.Error("run log inside the retention window was removed")
	}
	if f.exists(stale) {
		t.Error("run log older than the retention window survived")
	}
	if !f.exists(activePending) {
		t.Error("a pending run's log was removed while the run is still in flight")
	}
	if !f.exists(activeRunning) {
		t.Error("a running run's log was removed while the run is still in flight")
	}
}

// TestReapRunLogsTrimsToTheRunCeilingOldestFirst covers the count bound.
func TestReapRunLogsTrimsToTheRunCeilingOldestFirst(t *testing.T) {
	f := newRunLogFixture(t)

	oldest := f.seed("oldest", types.RunCompleted, 4*time.Hour)
	middle := f.seed("middle", types.RunCompleted, 3*time.Hour)
	newest := f.seed("newest", types.RunCompleted, time.Hour)

	reapRunLogs(f.db, f.root, evidenceReapPolicy{Retention: 14 * 24 * time.Hour, MaxRuns: 2}, time.Now())

	if f.exists(oldest) {
		t.Error("the oldest run log survived the run ceiling")
	}
	if !f.exists(middle) || !f.exists(newest) {
		t.Error("the two newest run logs should have been kept")
	}
}

// TestReapRunLogsKeepsEverythingWhenBothBoundsAreDisabled proves the operator
// escape hatch (the same one that already disables evidence reaping) really
// disables this reaper too.
func TestReapRunLogsKeepsEverythingWhenBothBoundsAreDisabled(t *testing.T) {
	f := newRunLogFixture(t)

	ancient := f.seed("ancient", types.RunCompleted, 365*24*time.Hour)
	recent := f.seed("recent", types.RunCompleted, time.Hour)

	reapRunLogs(f.db, f.root, evidenceReapPolicy{Retention: 0, MaxRuns: 0}, time.Now())

	if !f.exists(ancient) || !f.exists(recent) {
		t.Error("run logs were reaped even though both bounds are disabled")
	}
}

// TestReapRunLogsLeavesDaemonLogFilesAlone confirms the reaper only ever acts
// on the per-run subdirectories inside <NM_HOME>/logs, never the daemon's own
// log files that share the same root (daemon.log, cli.log, ...).
func TestReapRunLogsLeavesDaemonLogFilesAlone(t *testing.T) {
	f := newRunLogFixture(t)
	daemonLog := f.p.DaemonLog()
	if err := os.WriteFile(daemonLog, []byte("daemon output"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-365 * 24 * time.Hour)
	if err := os.Chtimes(daemonLog, when, when); err != nil {
		t.Fatal(err)
	}

	reapRunLogs(f.db, f.root, evidenceReapPolicy{Retention: time.Hour, MaxRuns: 1}, time.Now())

	if _, err := os.Stat(daemonLog); err != nil {
		t.Errorf("reaper removed a daemon log file, not a per-run log directory: %v", err)
	}
}

// TestRunCleanupReapsRunLogsAcrossTheWholeTree is the per-run half: cleaning
// up one finished run also converges the whole logs directory on the
// evidence retention budget, the same way it already does for evidence and
// leftover worktrees.
func TestRunCleanupReapsRunLogsAcrossTheWholeTree(t *testing.T) {
	f := newRunLogFixture(t)
	m := NewRunManager(f.db, f.p, nil)

	stale := f.seed("stale", types.RunCompleted, 30*24*time.Hour)
	fresh := f.seed("fresh", types.RunCompleted, time.Hour)

	cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
	cfg.Test.Evidence.Retention = 14 * 24 * time.Hour
	m.cleanupRunEvidence(cfg, fresh)

	if f.exists(stale) {
		t.Error("a stale run log survived cleanup of an unrelated run")
	}
	if !f.exists(fresh) {
		t.Error("a fresh run log was removed by the whole-tree reap")
	}
}
