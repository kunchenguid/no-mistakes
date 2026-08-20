//go:build unix

package gate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestEjectSweepsRecordedWorktreesBeforeRemovingThem is eject's half of the
// removal invariant, and it is the sharpest case of it: eject removes the
// directory AND deletes the run rows, so outside the default worktrees tree it
// destroys the last thing that could ever name that directory again. A process
// that escaped its process group and is standing in there - the case
// internal/procreap exists for - would then burn CPU holding a deleted cwd until
// the machine is rebooted.
func TestEjectSweepsRecordedWorktreesBeforeRemovingThem(t *testing.T) {
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
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	// A leftover run worktree in a directory of the operator's own, with an
	// escaped process standing in it.
	root := filepath.Join(t.TempDir(), "repo-runs")
	wtDir := filepath.Join(root, run.ID)
	if err := d.SetRunWorktreeDir(run.ID, wtDir); err != nil {
		t.Fatalf("record placement: %v", err)
	}
	leakedPID := startOrphanIn(t, wtDir)
	operatorPID := startOrphanIn(t, filepath.Join(root, "scratch-checkout"))

	if _, err := Eject(ctx, d, p, workDir); err != nil {
		t.Fatalf("eject: %v", err)
	}

	if fileExists(wtDir) {
		t.Errorf("recorded run worktree %q survived eject", wtDir)
	}
	if !pidGoneWithin(leakedPID, 10*time.Second) {
		t.Fatalf("orphan %d in the removed worktree survived eject, and no record can name that directory again", leakedPID)
	}
	if !processAlive(operatorPID) {
		t.Errorf("eject signalled %d in the operator's own directory", operatorPID)
	}
}

// startOrphanIn leaves a real long-lived process standing in dir whose parent has
// already exited, which is the shape a leaked pipeline child has by the time
// anyone notices it.
func startOrphanIn(t *testing.T, dir string) int {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create dir: %v", err)
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
	if !processAlive(pid) {
		t.Fatalf("orphan %d was not running", pid)
	}
	return pid
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func pidGoneWithin(pid int, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return !processAlive(pid)
}
