//go:build !windows

package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCaptureDirtyWork_RejectsFifoWithoutOpening(t *testing.T) {
	gate, wt, _, _ := newDirtyWorkGate(t, "fifo")
	fifo := filepath.Join(wt, "file.txt")
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	err := captureWithin(t, fifo, func() error {
		_, err := CaptureDirtyWork(context.Background(), DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
		return err
	})
	if !errors.Is(err, ErrDirtyWorkRejected) {
		t.Fatalf("fifo capture error = %v", err)
	}
	if _, statErr := os.Stat(fifo); statErr != nil {
		t.Fatal(statErr)
	}
	if _, err := Run(context.Background(), gate, "rev-parse", "--verify", "--quiet", DirtyWorkRef(dirtyWorkRunID)); err == nil {
		t.Fatal("fifo capture published a snapshot ref")
	}
}

func TestCaptureDirtyWork_RejectsUntrackedFifoWithoutOpening(t *testing.T) {
	gate, wt, _, _ := newDirtyWorkGate(t, "ufifo")
	fifo := filepath.Join(wt, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	err := captureWithin(t, fifo, func() error {
		_, err := CaptureDirtyWork(context.Background(), DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
		return err
	})
	if !errors.Is(err, ErrDirtyWorkRejected) {
		t.Fatalf("untracked fifo capture error = %v", err)
	}
	if _, statErr := os.Stat(fifo); statErr != nil {
		t.Fatal(statErr)
	}
	if _, err := Run(context.Background(), gate, "rev-parse", "--verify", "--quiet", DirtyWorkRef(dirtyWorkRunID)); err == nil {
		t.Fatal("untracked fifo published a snapshot ref")
	}
}

func TestCaptureDirtyWork_IgnoredFifoIsNotDirtyWork(t *testing.T) {
	gate, wt, _, _ := newDirtyWorkGate(t, "ififo")
	writeFile(t, filepath.Join(wt, ".gitignore"), "pipe\n")
	run(t, wt, "git", "add", "--", ".gitignore")
	run(t, wt, "git", "commit", "-m", "ignore pipe")
	fifo := filepath.Join(wt, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	var snap *DirtyWorkSnapshot
	err := captureWithin(t, fifo, func() error {
		var captureErr error
		snap, captureErr = CaptureDirtyWork(context.Background(), DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
		return captureErr
	})
	if err != nil || snap != nil {
		t.Fatalf("ignored fifo capture = %#v, %v", snap, err)
	}
}

func TestCaptureDirtyWork_SnapshotsSymlinkToFifo(t *testing.T) {
	ctx := context.Background()
	gate, wt, head, _ := newDirtyWorkGate(t, "fifo-link")
	writeFile(t, filepath.Join(wt, ".gitignore"), "pipe\n")
	fifo := filepath.Join(wt, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pipe", filepath.Join(wt, "see-pipe")); err != nil {
		t.Fatal(err)
	}
	var snap *DirtyWorkSnapshot
	err := captureWithin(t, fifo, func() error {
		var captureErr error
		snap, captureErr = CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
		return captureErr
	})
	if err != nil {
		t.Fatalf("symlink to fifo: %v", err)
	}
	restored := filepath.Join(t.TempDir(), "restored-fifo-link")
	if err := WorktreeAdd(ctx, gate, restored, head); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = WorktreeRemove(ctx, gate, restored) })
	if err := RestoreDirtyWork(ctx, restored, snap.Commit); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.Readlink(filepath.Join(restored, "see-pipe"))
	if err != nil || got != "pipe" {
		t.Fatalf("restored see-pipe = %q, %v", got, err)
	}
}

func captureWithin(t *testing.T, fifo string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- fn()
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		f, openErr := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if openErr == nil {
			f.Close()
			<-done
		}
		t.Fatal("capture blocked on a fifo")
	}
	return nil
}
