package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/procreap"
)

func TestCleanupSweepWaitsForActiveCaptureOwnership(t *testing.T) {
	for _, path := range []string{"run", "reap", "recorded"} {
		t.Run(path, func(t *testing.T) {
			f := newStartupCleanupFixture(t)
			wt := f.checkout(t, path == "recorded")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			command := shellQuoteForTest(filepath.ToSlash(executable)) + " --nm-startup-cleanup-filter smudge-pass " + shellQuoteForTest(filepath.ToSlash(f.controls))
			gitCmd(t, f.gate, "config", "filter.slow.smudge", command)
			gitCmd(t, wt.dir, "add", ".gitattributes")
			gitCmd(t, wt.dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "committed filter")
			if err := os.WriteFile(filepath.Join(wt.dir, "unique.txt"), []byte("captured unique bytes\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			var sweeps atomic.Int32
			sweepRunWorktrees = func(ctx context.Context, root string, worktrees []procreap.Worktree, reason string) {
				sweeps.Add(1)
				procreap.SweepRunWorktreesContext(ctx, root, worktrees, reason)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			done := make(chan struct{})
			var snap *git.DirtyWorkSnapshot
			var captureErr error
			go func() {
				defer close(done)
				captureErr = git.WithDirtyWorkCheckouts(ctx, []string{wt.dir}, func() error {
					var err error
					snap, err = git.CaptureDirtyWork(ctx, git.DirtyWorkRequest{GateDir: wt.gateDir, Worktree: wt.dir, RunID: wt.runID})
					return err
				})
			}()
			t.Cleanup(func() {
				cancel()
				f.release(t, wt)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("fixture capture did not stop")
				}
			})
			waitForStartupCleanupFilter(t, f.entered(wt), done)
			waitCtx, stopWaiting := context.WithTimeout(context.Background(), 2*time.Second)
			defer stopWaiting()
			waitingDone := make(chan struct{})
			go func() {
				defer close(waitingDone)
				switch path {
				case "run":
					NewRunManager(f.d, f.p, nil).removeRunWorktree(waitCtx, wt.repoID, wt.runID, wt.gateDir, wt.dir, "concurrent capture")
				case "reap":
					reapWorktrees(waitCtx, f.d, f.p, worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour))
				case "recorded":
					cleanupOrphanWorktrees(waitCtx, f.d, f.p, []db.RunWorktree{{RepoID: wt.repoID, RunID: wt.runID, Dir: wt.dir}})
				}
			}()
			t.Cleanup(func() {
				stopWaiting()
				select {
				case <-waitingDone:
				case <-time.After(10 * time.Second):
					t.Error("waiting cleanup did not stop")
				}
			})
			select {
			case <-waitingDone:
			case <-time.After(8 * time.Second):
				t.Fatal("cleanup ownership wait exceeded its deadline")
			}
			if sweeps.Load() != 0 {
				t.Fatal("overlapping cleanup swept an actively captured checkout")
			}
			select {
			case <-done:
				t.Fatal("overlapping cleanup interrupted the active Git filter")
			default:
			}
			f.release(t, wt)
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("capture did not finish after its filter was released")
			}
			if captureErr != nil || snap == nil {
				t.Fatalf("owned capture = %#v, %v", snap, captureErr)
			}
			if got := gitOutput(t, wt.gateDir, "show", snap.Commit+"^3:unique.txt"); got != "captured unique bytes" {
				t.Fatalf("capture lost unique bytes: %q", got)
			}
			if _, err := os.Stat(wt.dir); err != nil {
				t.Fatalf("overlapping maintenance removed the checkout: %v", err)
			}
		})
	}
}
