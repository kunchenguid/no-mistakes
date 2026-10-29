package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDirtyWorkScratchCleanupUsesCaptureContext(t *testing.T) {
	for _, phase := range []string{"write", "confirm", "clean"} {
		t.Run(phase, func(t *testing.T) {
			gate, wt, _, _ := newDirtyWorkGate(t, "cancelled scratch")
			var snap *DirtyWorkSnapshot
			if phase != "clean" {
				writeFile(t, filepath.Join(wt, "file.txt"), "unique retained bytes\n")
			}
			if phase == "confirm" {
				var err error
				snap, err = CaptureDirtyWork(context.Background(), DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
				if err != nil || snap == nil {
					t.Fatalf("initial capture = %#v, %v", snap, err)
				}
			}
			root := t.TempDir()
			t.Setenv("TMPDIR", root)
			t.Setenv("TMP", root)
			t.Setenv("TEMP", root)
			t.Cleanup(func() { _ = ReapDirtyWorkScratch(context.Background()) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			previousRef := dirtyWorkBeforeRefUpdate
			previousCheck := dirtyWorkAfterBookkeeping
			t.Cleanup(func() {
				dirtyWorkBeforeRefUpdate = previousRef
				dirtyWorkAfterBookkeeping = previousCheck
			})
			var retained string
			stop := func() {
				entries, err := os.ReadDir(root)
				if err != nil || len(entries) != 1 || !entries[0].IsDir() {
					t.Fatal("capture did not create its isolated scratch directory")
				}
				retained = filepath.Join(root, entries[0].Name(), "unfinished.txt")
				writeFile(t, retained, "retained scratch bytes\n")
				if err := ReapDirtyWorkScratch(context.Background()); err != nil {
					t.Fatal(err)
				}
				if string(mustRead(t, retained)) != "retained scratch bytes\n" {
					t.Fatal("reclamation changed an active capture's scratch")
				}
				cancel()
			}
			if phase == "write" {
				dirtyWorkBeforeRefUpdate = stop
			} else {
				dirtyWorkAfterBookkeeping = func(path string) {
					if path == wt {
						stop()
					}
				}
			}
			var err error
			if phase == "confirm" {
				err = DirtyWorkStillMatches(ctx, wt, snap)
			} else {
				_, err = CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
			}
			if err == nil || retained == "" || ctx.Err() == nil {
				t.Fatalf("capture did not observe cancellation: %v", err)
			}
			if got := string(mustRead(t, retained)); got != "retained scratch bytes\n" {
				t.Fatal("cancelled cleanup changed unfinished scratch bytes")
			}
			if phase != "confirm" {
				assertNoDirtySnapshot(t, gate)
			}
			if phase != "clean" && string(mustRead(t, filepath.Join(wt, "file.txt"))) != "unique retained bytes\n" {
				t.Fatal("cancelled cleanup changed source work")
			}
			if err := ReapDirtyWorkScratch(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled reclamation = %v", err)
			}
			if phase != "write" {
				if err := ReapDirtyWorkScratch(context.Background()); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(retained); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("inactive scratch survived later reclamation: %v", err)
				}
			}
			if snap != nil && run(t, gate, "git", "rev-parse", snap.Ref) != snap.Commit {
				t.Fatal("scratch reclamation changed the verified snapshot")
			}
			dirtyWorkBeforeRefUpdate = previousRef
			dirtyWorkAfterBookkeeping = previousCheck
			retry, err := CaptureDirtyWork(context.Background(), DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
			if err != nil || (phase != "clean" && retry == nil) {
				t.Fatalf("retry after scratch reclamation = %#v, %v", retry, err)
			}
			if _, err := os.Stat(retained); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("capture retry abandoned earlier scratch: %v", err)
			}
		})
	}
}

func TestReapDirtyWorkScratchPreservesActiveAndUnownedDirectories(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	t.Setenv("TMP", root)
	t.Setenv("TEMP", root)
	active, err := newDirtyWorkScratch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		active.close(context.Background())
		if _, err := os.Stat(active.path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("completed scratch survived reclamation: %v", err)
		}
	})
	unowned := filepath.Join(root, "nm-dirty-work-unrecorded")
	if err := os.Mkdir(unowned, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{active.path, unowned} {
		writeFile(t, filepath.Join(path, "unique.txt"), "preserved bytes\n")
	}
	if err := ReapDirtyWorkScratch(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{active.path, unowned} {
		if string(mustRead(t, filepath.Join(path, "unique.txt"))) != "preserved bytes\n" {
			t.Fatal("reclamation touched active or unowned data")
		}
	}
}

func TestReapDirtyWorkScratchRetainsAmbiguousOwnership(t *testing.T) {
	for _, kind := range []string{"replacement", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("TMPDIR", root)
			t.Setenv("TMP", root)
			t.Setenv("TEMP", root)
			scratch, err := newDirtyWorkScratch(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			scratch.close(ctx)
			t.Cleanup(func() { _ = ReapDirtyWorkScratch(context.Background()) })
			preserved := filepath.Join(root, "verified-recovery-evidence")
			if err := os.Rename(scratch.path, preserved); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(preserved, "proof.txt"), "recovery bytes\n")
			if kind == "symlink" {
				if err := os.Symlink(preserved, scratch.path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			} else {
				if err := os.Mkdir(scratch.path, 0o700); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(scratch.path, "proof.txt"), "unrelated bytes\n")
			}
			if err := ReapDirtyWorkScratch(context.Background()); !errors.Is(err, os.ErrInvalid) {
				t.Fatalf("ambiguous ownership = %v, want refusal", err)
			}
			if string(mustRead(t, filepath.Join(preserved, "proof.txt"))) != "recovery bytes\n" {
				t.Fatal("reclamation changed recovery evidence")
			}
			if _, err := os.Lstat(scratch.path); err != nil {
				t.Fatalf("reclamation removed an unrelated replacement: %v", err)
			}
		})
	}
}

type scratchRemovalContext struct {
	context.Context
	cancel context.CancelFunc
	path   string
	count  int
}

func (c *scratchRemovalContext) Err() error {
	if entries, err := os.ReadDir(c.path); err == nil && len(entries) < c.count {
		c.cancel()
	}
	return c.Context.Err()
}

func TestReapDirtyWorkScratchRetriesInterruptedRemovalWithinBudget(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	t.Setenv("TMP", root)
	t.Setenv("TEMP", root)
	scratch, err := newDirtyWorkScratch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		writeFile(t, filepath.Join(scratch.path, fmt.Sprintf("partial-%02d", i)), "scratch copy\n")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	budget := &scratchRemovalContext{Context: ctx, cancel: cancel, path: scratch.path, count: 40}
	scratch.close(budget)
	t.Cleanup(func() { _ = ReapDirtyWorkScratch(context.Background()) })
	entries, err := os.ReadDir(scratch.path)
	if err != nil || len(entries) == 0 || len(entries) == 40 || ctx.Err() == nil {
		t.Fatalf("cleanup did not stop partway through removal: %d entries, %v", len(entries), err)
	}
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if err := ReapDirtyWorkScratch(expired); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exhausted reclamation budget = %v", err)
	}
	if remaining, err := os.ReadDir(scratch.path); err != nil || len(remaining) != len(entries) {
		t.Fatal("exhausted reclamation budget removed more scratch")
	}
	if err := ReapDirtyWorkScratch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scratch.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("later reclamation did not finish interrupted removal: %v", err)
	}
}
