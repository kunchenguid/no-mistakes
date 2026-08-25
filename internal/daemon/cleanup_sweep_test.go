package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/procreap"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestCleanupOrphanWorktreesSweepsEveryRemovableDirectoryInOneSnapshot bounds
// what startup cleanup pays for the sweep that has to precede every removal.
// One snapshot per directory makes the daemon slower to start the more a crash
// left behind: a scoped sweep has no age floor, so each snapshot enumerates
// every process on the machine (a whole-machine lsof on darwin, capped at 10s),
// and the whole pass runs before the socket is bound, against the startup
// budget. Per-directory decisions must not change: an active run's worktree is
// neither swept nor removed.
func TestCleanupOrphanWorktreesSweepsEveryRemovableDirectoryInOneSnapshot(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	workingPath := filepath.Join(t.TempDir(), "checkout")
	if err := os.MkdirAll(workingPath, 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepoWithID("repo1", workingPath, "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}

	defaultTree := filepath.Join(p.WorktreesDir(), repo.ID)

	// Two finished runs in the default tree, one in a directory of the
	// operator's own, and one run still executing.
	firstDefault := placeCleanupRun(t, d, repo.ID, defaultTree, types.RunFailed)
	secondDefault := placeCleanupRun(t, d, repo.ID, defaultTree, types.RunCompleted)
	operatorRoot := filepath.Join(t.TempDir(), "repo-runs")
	recorded := placeCleanupRun(t, d, repo.ID, operatorRoot, types.RunFailed)
	activeDefault := placeCleanupRun(t, d, repo.ID, defaultTree, types.RunRunning)

	var sweeps [][]procreap.Worktree
	original := sweepRunWorktrees
	t.Cleanup(func() { sweepRunWorktrees = original })
	sweepRunWorktrees = func(worktreesRoot string, wts []procreap.Worktree, reason string) {
		sweeps = append(sweeps, wts)
	}

	cleanupOrphanWorktrees(d, p, leftoverRecordedRunWorktrees(d, p))

	if len(sweeps) != 1 {
		t.Fatalf("startup cleanup ran %d sweeps for 3 removable directories, want 1", len(sweeps))
	}
	swept := map[string]bool{}
	for _, wt := range sweeps[0] {
		swept[wt.Dir] = true
	}
	for _, dir := range []string{firstDefault, secondDefault, recorded} {
		if !swept[dir] {
			t.Errorf("removable worktree %q was not swept before removal", dir)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("worktree %q survived cleanup, stat err: %v", dir, err)
		}
	}
	if swept[activeDefault] {
		t.Errorf("cleanup swept %q, whose run is still executing", activeDefault)
	}
	if _, err := os.Stat(activeDefault); err != nil {
		t.Errorf("cleanup removed the worktree of a running run: %v", err)
	}
}

// placeCleanupRun records a run placed in root, creates its directory, and
// leaves it in the given status.
func placeCleanupRun(t *testing.T, d *db.DB, repoID, root string, status types.RunStatus) string {
	t.Helper()
	run, err := d.InsertRun(repoID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, run.ID)
	if err := d.SetRunWorktreeDir(run.ID, dir); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, status); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
