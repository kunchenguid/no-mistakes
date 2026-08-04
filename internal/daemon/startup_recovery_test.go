package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRecoverOnStartup_DoesNotDeleteActiveRunWorktree is the regression test
// for the second half of the duplicate-daemon wedge: startup cleanup used to
// remove every worktree directory under the shared root with no check of the
// owning run's status, so a duplicate daemon's cleanup could delete the
// checkout out from under a pipeline that was still actively running in it
// (observed live as "chdir .../worktrees/...: no such file or directory").
// A worktree whose run row is pending or running must survive cleanup.
//
// This exercises cleanupOrphanWorktrees directly (rather than the full
// recoverOnStartup) because RecoverStaleRuns, which runs first in
// production, unconditionally marks every pending/running run failed - so by
// design there is no pending/running row left by the time cleanup runs in
// the normal single-daemon path. Testing cleanupOrphanWorktrees in isolation
// verifies its DB-aware skip logic as defense in depth, independent of
// whatever recovery step runs before it.
func TestRecoverOnStartup_DoesNotDeleteActiveRunWorktree(t *testing.T) {
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

	activeWT := p.WorktreeDir(repo.ID, activeRun.ID)
	if err := os.MkdirAll(activeWT, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activeWT+"/marker", []byte("still running"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A terminal run's worktree, for contrast: cleanup should remove this one.
	terminalRun, err := d.InsertRun(repo.ID, "old-branch", "headsha2", "basesha2")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(terminalRun.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	terminalWT := p.WorktreeDir(repo.ID, terminalRun.ID)
	if err := os.MkdirAll(terminalWT, 0o755); err != nil {
		t.Fatal(err)
	}

	cleanupOrphanWorktrees(d, p)

	if _, err := os.Stat(activeWT); err != nil {
		t.Fatalf("active run worktree must survive cleanup, got: %v", err)
	}
	if _, err := os.Stat(terminalWT); !os.IsNotExist(err) {
		t.Fatalf("terminal run worktree should have been cleaned up, stat err: %v", err)
	}

	got, err := d.GetRun(activeRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunPending {
		t.Fatalf("expected active run to remain pending, got %s", got.Status)
	}
}

func TestCleanupTerminalRunPRBaseRefs_RetainsRecoverableRefs(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	source := t.TempDir()
	gitCmd(t, source, "init", "--initial-branch=main")
	gitCmd(t, source, "config", "user.email", "test@test.com")
	gitCmd(t, source, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, source, "add", "README.md")
	gitCmd(t, source, "commit", "-m", "initial")

	repo, err := database.InsertRepoWithID("private-ref-cleanup", source, "https://example.com/owner/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(repo.ID)
	gitCmd(t, "", "clone", "--bare", source, gateDir)
	head := gitOutput(t, source, "rev-parse", "HEAD")
	active, err := database.InsertRun(repo.ID, "active", head, head)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := database.InsertRun(repo.ID, "terminal", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(terminal.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{active.ID, terminal.ID} {
		for _, ref := range []string{git.RunPRBaseRef(runID), git.RunPRBaseMonitorRef(runID)} {
			gitCmd(t, gateDir, "update-ref", ref, head)
		}
	}

	cleanupTerminalRunPRBaseRefs(database, gateDir, active.ID)
	cleanupTerminalRunPRBaseRefs(database, gateDir, terminal.ID)

	for _, ref := range []string{git.RunPRBaseRef(active.ID), git.RunPRBaseMonitorRef(active.ID)} {
		if exists, err := git.RefExists(context.Background(), gateDir, ref); err != nil || !exists {
			t.Fatalf("recoverable ref %s was removed: exists=%t err=%v", ref, exists, err)
		}
	}
	for _, ref := range []string{git.RunPRBaseRef(terminal.ID), git.RunPRBaseMonitorRef(terminal.ID)} {
		if exists, err := git.RefExists(context.Background(), gateDir, ref); err != nil || exists {
			t.Fatalf("terminal ref %s remains: exists=%t err=%v", ref, exists, err)
		}
	}
}

// TestRunWithOptions_RequiresSingletonLockBeforeRecovery proves the ordering
// the fix depends on: when another process already holds the singleton lock
// for this root, RunWithOptions must fail before ever calling
// RecoverStaleRuns, so a duplicate daemon can never mark a live daemon's
// active runs as crashed.
func TestRunWithOptions_RequiresSingletonLockBeforeRecovery(t *testing.T) {
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
	run, err := d.InsertRun(repo.ID, "feature", "headsha", "basesha")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate another live daemon already owning this root.
	lock, err := acquireSingletonLock(p)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	if err := RunWithOptions(p, d, nil); err == nil {
		t.Fatal("expected RunWithOptions to fail while the singleton lock is held elsewhere")
	} else if !errors.Is(err, ErrSingletonLockHeld) {
		t.Fatalf("expected ErrSingletonLockHeld, got %v", err)
	}

	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunPending {
		t.Fatalf("recovery must not have run: expected run to remain pending, got %s", got.Status)
	}
}
