package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// worktreeReapFixture builds a DB plus a default worktrees tree and returns
// helpers for seeding leftover run-worktree directories with a chosen age and
// status. Directories are plain os.MkdirAll, not real git worktrees: removal
// goes through removeOrphanWorktree, whose git.WorktreeRemove attempt fails
// against the fixture's non-existent gate repo and falls back to
// os.RemoveAll - the same fallback path startup cleanup already relies on
// (see TestCleanupOrphanWorktreesSweepsEveryRemovableDirectoryInOneSnapshot).
type worktreeReapFixture struct {
	t    *testing.T
	db   *db.DB
	p    *paths.Paths
	repo *db.Repo
}

func newWorktreeReapFixture(t *testing.T) *worktreeReapFixture {
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
	return &worktreeReapFixture{t: t, db: d, p: p, repo: repo}
}

// seed records a run in the given status and creates its default-tree
// worktree directory, aged by backdating the directory's modification time.
func (f *worktreeReapFixture) seed(branch string, status types.RunStatus, age time.Duration) string {
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
	dir := f.p.WorktreeDir(f.repo.ID, run.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(dir, when, when); err != nil {
		f.t.Fatal(err)
	}
	return run.ID
}

func (f *worktreeReapFixture) exists(runID string) bool {
	f.t.Helper()
	_, err := os.Stat(f.p.WorktreeDir(f.repo.ID, runID))
	return err == nil
}

// TestReapWorktreesHonorsRetentionAndSparesActiveRuns is the core reaper
// contract: a leftover worktree past the retention window is removed, one
// inside it is kept, and a run still in flight is never touched at any age.
func TestReapWorktreesHonorsRetentionAndSparesActiveRuns(t *testing.T) {
	f := newWorktreeReapFixture(t)

	fresh := f.seed("fresh", types.RunCompleted, time.Hour)
	stale := f.seed("stale", types.RunFailed, 30*24*time.Hour)
	activePending := f.seed("active-pending", types.RunPending, 30*24*time.Hour)
	activeRunning := f.seed("active-running", types.RunRunning, 30*24*time.Hour)

	reapWorktrees(f.db, f.p, worktreeReapPolicy{Retention: 14 * 24 * time.Hour}, time.Now())

	if !f.exists(fresh) {
		t.Error("a worktree inside the retention window was removed")
	}
	if f.exists(stale) {
		t.Error("a worktree older than the retention window survived")
	}
	if !f.exists(activePending) {
		t.Error("a pending run's worktree was removed while the run is still in flight")
	}
	if !f.exists(activeRunning) {
		t.Error("a running run's worktree was removed while the run is still in flight")
	}
}

// TestReapWorktreesTrimsToTheRunCeilingOldestFirst covers the second bound: a
// burst of leftovers that all land inside the retention window still cannot
// grow the tree without limit.
func TestReapWorktreesTrimsToTheRunCeilingOldestFirst(t *testing.T) {
	f := newWorktreeReapFixture(t)

	oldest := f.seed("oldest", types.RunCompleted, 4*time.Hour)
	middle := f.seed("middle", types.RunCompleted, 3*time.Hour)
	newest := f.seed("newest", types.RunCompleted, time.Hour)

	reapWorktrees(f.db, f.p, worktreeReapPolicy{Retention: 14 * 24 * time.Hour, MaxRuns: 2}, time.Now())

	if f.exists(oldest) {
		t.Error("the oldest leftover worktree survived the run ceiling")
	}
	if !f.exists(middle) || !f.exists(newest) {
		t.Error("the two newest leftover worktrees should have been kept")
	}
}

// TestReapWorktreesKeepsEverythingWhenBothBoundsAreDisabled proves the
// operator escape hatch really disables reaping.
func TestReapWorktreesKeepsEverythingWhenBothBoundsAreDisabled(t *testing.T) {
	f := newWorktreeReapFixture(t)

	ancient := f.seed("ancient", types.RunCompleted, 365*24*time.Hour)
	recent := f.seed("recent", types.RunCompleted, time.Hour)

	reapWorktrees(f.db, f.p, worktreeReapPolicy{Retention: 0, MaxRuns: 0}, time.Now())

	if !f.exists(ancient) || !f.exists(recent) {
		t.Error("worktrees were reaped even though both bounds are disabled")
	}
}

// TestWorktreeReapPolicyForUsesGlobalConfigAndDefaults keeps the daemon's
// resolution aligned with the global-only config contract (config.Worktree).
func TestWorktreeReapPolicyForUsesGlobalConfigAndDefaults(t *testing.T) {
	if got := worktreeReapPolicyFor(nil); got.Retention != config.DefaultWorktreeRetention || got.MaxRuns != config.DefaultWorktreeMaxRuns {
		t.Errorf("nil global config = %+v, want the built-in defaults", got)
	}

	global, err := config.LoadGlobalFromBytes([]byte("worktree:\n  retention: 100h\n  max_runs: 7\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := worktreeReapPolicyFor(global)
	if got.Retention != 100*time.Hour || got.MaxRuns != 7 {
		t.Errorf("policy = %+v, want retention 100h and max_runs 7", got)
	}
}

// TestRunCleanupReapsLeftoverWorktreesAcrossTheWholeTree is the per-run half:
// cleaning up one finished run also converges the whole default tree on the
// worktree retention budget, the same way it already does for evidence.
func TestRunCleanupReapsLeftoverWorktreesAcrossTheWholeTree(t *testing.T) {
	f := newWorktreeReapFixture(t)
	m := NewRunManager(f.db, f.p, nil)

	stale := f.seed("stale", types.RunCompleted, 30*24*time.Hour)
	fresh := f.seed("fresh", types.RunCompleted, time.Hour)

	cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
	cfg.Worktree.Retention = 14 * 24 * time.Hour
	m.cleanupRunEvidence(cfg, fresh)

	if f.exists(stale) {
		t.Error("a stale leftover worktree survived cleanup of an unrelated run")
	}
	if !f.exists(fresh) {
		t.Error("a fresh worktree was removed by the whole-tree reap")
	}
}

// TestRunCleanupWorktreeReapIsSafeWithoutConfig keeps the cleanup path from
// ever being the thing that fails a finished run when config failed to load.
func TestRunCleanupWorktreeReapIsSafeWithoutConfig(t *testing.T) {
	f := newWorktreeReapFixture(t)
	m := NewRunManager(f.db, f.p, nil)

	kept := f.seed("kept", types.RunCompleted, time.Minute)

	m.cleanupRunEvidence(nil, "run-that-never-existed")

	if !f.exists(kept) {
		t.Error("cleanup with no config removed a fresh leftover worktree")
	}
}

// TestRemoveOrphanWorktreeReportsRemovalFailure pins the fix for the
// inflated-count review finding: removeOrphanWorktree must report false, not
// just log a warning, when both git worktree remove and its os.RemoveAll
// fallback fail, so a caller counting removals (reapWorktrees) never claims a
// directory is gone when it is still on disk.
func TestRemoveOrphanWorktreeReportsRemovalFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission bits do not block removal the same way on windows")
	}
	tmp := t.TempDir()
	wtDir := filepath.Join(tmp, "wt")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Strip write permission on the parent directory so os.RemoveAll cannot
	// unlink wtDir's entry, after git.WorktreeRemove has already failed
	// against the nonexistent gate repo below.
	parent := filepath.Dir(wtDir)
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o755) })

	wt := orphanWorktree{gateDir: filepath.Join(tmp, "nonexistent-gate"), dir: wtDir, repoID: "repo1", runID: "run1"}
	if removeOrphanWorktree(context.Background(), wt) {
		t.Fatal("removeOrphanWorktree reported success for a directory that is still on disk")
	}
	if _, err := os.Stat(wtDir); err != nil {
		t.Fatalf("worktree directory should still exist after a failed removal: %v", err)
	}
}
