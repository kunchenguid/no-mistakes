//go:build unix

package daemon

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestSweepOrphanRunProcessesReapsFinishedRunAndSparesActiveOne is the daemon
// wiring for the leaked-process class: a predecessor daemon that died mid-run
// never got to tear anything down, so its children are still standing in
// worktrees that no run owns. Startup must reap those, and must leave alone
// anything standing in a worktree whose run is still pending or running -
// killing there would take down a live pipeline.
func TestSweepOrphanRunProcessesReapsFinishedRunAndSparesActiveOne(t *testing.T) {
	// The startup sweep normally refuses to touch anything young, so a run
	// starting concurrently with daemon startup is never mistaken for a leak.
	// These fixtures are seconds old, so the floor is lowered for the test.
	orig := orphanProcessMinAge
	orphanProcessMinAge = 0
	t.Cleanup(func() { orphanProcessMinAge = orig })

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	repo, err := d.InsertRepoWithID("repo1", "/nonexistent/work", "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}

	activeRun, err := d.InsertRun(repo.ID, "feature", "headsha", "basesha")
	if err != nil {
		t.Fatal(err)
	}
	if activeRun.Status != types.RunPending {
		t.Fatalf("expected new run to default to pending, got %s", activeRun.Status)
	}
	finishedRun, err := d.InsertRun(repo.ID, "old-branch", "headsha2", "basesha2")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(finishedRun.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}

	activePID := startOrphanInWorktree(t, p.WorktreeDir(repo.ID, activeRun.ID))
	leakedPID := startOrphanInWorktree(t, p.WorktreeDir(repo.ID, finishedRun.ID))

	sweepOrphanRunProcesses(d, p)

	if !pidGoneWithin(leakedPID, 10*time.Second) {
		t.Fatalf("orphan %d in the finished run's worktree survived the startup sweep", leakedPID)
	}
	if !processIsAlive(activePID) {
		t.Fatalf("orphan %d in an active run's worktree must not be swept", activePID)
	}
}

// TestSweepRunWorktreeProcessesReapsLeakedChildAtRunCleanup covers the other
// call site: when a run's goroutine finishes, anything still standing in that
// run's worktree is terminated before the directory is removed, and another
// run's worktree is never touched.
func TestSweepRunWorktreeProcessesReapsLeakedChildAtRunCleanup(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(nil, p, nil)

	finished := p.WorktreeDir("repo1", "run1")
	other := p.WorktreeDir("repo1", "run2")
	leakedPID := startOrphanInWorktree(t, finished)
	otherPID := startOrphanInWorktree(t, other)

	m.sweepRunWorktreeProcesses(finished)

	if !pidGoneWithin(leakedPID, 10*time.Second) {
		t.Fatalf("orphan %d in the finished run's worktree survived run cleanup", leakedPID)
	}
	if !processIsAlive(otherPID) {
		t.Fatalf("run cleanup reached another run's worktree and killed %d", otherPID)
	}
}

// startOrphanInWorktree leaves a real long-lived process standing in dir whose
// parent has already exited, which is exactly the shape a leaked pipeline
// child has by the time anyone notices it.
func startOrphanInWorktree(t *testing.T, dir string) int {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create worktree dir: %v", err)
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 300 >/dev/null 2>&1 & echo $!")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("spawn orphan: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		t.Fatalf("orphan pid from %q: %v", out, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if !processIsAlive(pid) {
		t.Fatalf("orphan %d was not running", pid)
	}
	return pid
}

func processIsAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func pidGoneWithin(pid int, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if !processIsAlive(pid) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return !processIsAlive(pid)
}
