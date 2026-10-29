//go:build !windows

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestRemoveRunWorktreeKeepsFifo(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, headSHA := setupTestGitRepo(t, p, d, "repo1")
	const runID = "01JZ8XQ7V6K9M3B0T5N2R4C8YF"
	wtDir := filepath.Join(t.TempDir(), "fifo-wt")
	gate := p.RepoDir("repo1")
	gitCmd(t, gate, "worktree", "add", "--detach", wtDir, headSHA)
	fifo := filepath.Join(wtDir, "test.txt")
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}

	removeWithin(t, fifo, func() {
		NewRunManager(d, p, nil).removeRunWorktree(context.Background(), "repo1", runID, gate, wtDir, "test")
	})
	if _, err := os.Stat(wtDir); err != nil {
		t.Fatalf("fifo checkout was removed: %v", err)
	}
}

func TestRemoveOrphanWorktreeKeepsFifo(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, headSHA := setupTestGitRepo(t, p, d, "repo1")
	const runID = "01JZ8XQ7V6K9M3B0T5N2R4C8YG"
	wtDir := filepath.Join(t.TempDir(), "orphan-fifo")
	gate := p.RepoDir("repo1")
	gitCmd(t, gate, "worktree", "add", "--detach", wtDir, headSHA)
	fifo := filepath.Join(wtDir, "test.txt")
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}

	var removed int
	removeWithin(t, fifo, func() {
		removed = removeOrphanWorktrees(context.Background(), p.WorktreesDir(), []orphanWorktree{{
			gateDir: gate,
			dir:     wtDir,
			repoID:  "repo1",
			runID:   runID,
		}}, "test")
	})
	if removed != 0 {
		t.Fatal("orphan removal deleted a checkout that contains a fifo")
	}
	if _, err := os.Stat(fifo); err != nil {
		t.Fatal(err)
	}
}

func removeWithin(t *testing.T, fifo string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			f.Close()
			<-done
		}
		t.Fatal("removal blocked on a fifo")
	}
}
