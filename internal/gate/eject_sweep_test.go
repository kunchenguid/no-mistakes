package gate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/procreap"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestEjectSweepsEveryReachablePlacementInOneSweep bounds what eject pays for
// the sweep it has to do. A repository's run rows are never pruned, so one sweep
// per recorded placement means one process-table read per run the repository has
// ever placed outside the default tree - thousands of `ps` and cwd passes for an
// eject that has one leftover directory to clean up. One sweep answers all of
// them, and a terminal run whose directory is already gone is not in it: its own
// removal swept it, and nothing can be standing in a directory that no longer
// exists for a run that ended.
func TestEjectSweepsEveryReachablePlacementInOneSweep(t *testing.T) {
	workDir := setupTestRepo(t)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	repo, _, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	root := filepath.Join(t.TempDir(), "repo-runs")
	leftover := placeRun(t, d, repo.ID, root, types.RunCompleted, true)
	active := placeRun(t, d, repo.ID, root, types.RunRunning, false)
	finished := placeRun(t, d, repo.ID, root, types.RunCompleted, false)

	var sweeps [][]procreap.Worktree
	original := sweepRunWorktrees
	t.Cleanup(func() { sweepRunWorktrees = original })
	sweepRunWorktrees = func(worktreesRoot string, wts []procreap.Worktree, reason string) {
		sweeps = append(sweeps, wts)
	}

	if _, err := Eject(ctx, d, p, workDir); err != nil {
		t.Fatalf("eject: %v", err)
	}

	if len(sweeps) != 1 {
		t.Fatalf("eject ran %d sweeps for 3 recorded placements, want 1", len(sweeps))
	}
	swept := map[string]bool{}
	for _, wt := range sweeps[0] {
		swept[wt.Dir] = true
	}
	if !swept[leftover] {
		t.Errorf("leftover worktree %q on disk was not swept before removal", leftover)
	}
	if !swept[active] {
		t.Errorf("worktree %q of a run that never reached a terminal state was not swept", active)
	}
	if swept[finished] {
		t.Errorf("eject swept %q, a finished run's directory that is already gone", finished)
	}
	if fileExists(leftover) {
		t.Errorf("recorded run worktree %q survived eject", leftover)
	}
}

// TestEjectSweepsTheDefaultTreeBeforeRemovingIt holds the sweep-before-removal
// discipline to both halves of eject. Which placement a run landed in says
// nothing about whether it leaked a process that escaped its group, so removing
// the default tree unswept leaves that process burning CPU on a deleted cwd until
// some later daemon startup happens to sweep the tree by shape - the exact cost
// the eject sweep exists to eliminate rather than defer.
func TestEjectSweepsTheDefaultTreeBeforeRemovingIt(t *testing.T) {
	workDir := setupTestRepo(t)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	repo, _, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	// A run in the default tree: it records no placement of its own, which is
	// what the default placement looks like on the row.
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatalf("set run status: %v", err)
	}
	defaultDir := p.WorktreeDir(repo.ID, run.ID)
	if err := os.MkdirAll(defaultDir, 0o755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}

	var sweeps int
	sweptWhileOnDisk := map[string]bool{}
	original := sweepRunWorktrees
	t.Cleanup(func() { sweepRunWorktrees = original })
	sweepRunWorktrees = func(worktreesRoot string, wts []procreap.Worktree, reason string) {
		sweeps++
		for _, wt := range wts {
			sweptWhileOnDisk[wt.Dir] = fileExists(wt.Dir)
		}
	}

	if _, err := Eject(ctx, d, p, workDir); err != nil {
		t.Fatalf("eject: %v", err)
	}

	onDisk, swept := sweptWhileOnDisk[defaultDir]
	if !swept {
		t.Errorf("default-placement worktree %q was removed without a sweep", defaultDir)
	} else if !onDisk {
		t.Errorf("default-placement worktree %q was swept only after its removal", defaultDir)
	}
	if sweeps != 1 {
		t.Errorf("eject ran %d sweeps, want one snapshot covering both halves", sweeps)
	}
	if fileExists(defaultDir) {
		t.Errorf("default-placement worktree %q survived eject", defaultDir)
	}
}

// placeRun records a run whose worktree the operator's own directory holds,
// optionally creating that directory.
func placeRun(t *testing.T, d *db.DB, repoID, root string, status types.RunStatus, onDisk bool) string {
	t.Helper()
	run, err := d.InsertRun(repoID, "feature", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	dir := filepath.Join(root, run.ID)
	if err := d.SetRunWorktreeDir(run.ID, dir); err != nil {
		t.Fatalf("record placement: %v", err)
	}
	if err := d.UpdateRunStatus(run.ID, status); err != nil {
		t.Fatalf("set run status: %v", err)
	}
	if onDisk {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create worktree: %v", err)
		}
	}
	return dir
}
