package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWorktreeAddAndRemove(t *testing.T) {
	// create a bare repo with at least one commit by pushing from a regular repo
	ctx := context.Background()
	src := initTestRepo(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	run(t, bare, "git", "config", "core.autocrlf", "false")
	run(t, src, "git", "remote", "add", "bare", bare)
	run(t, src, "git", "push", "bare", "HEAD:refs/heads/main")

	sha := run(t, src, "git", "rev-parse", "HEAD")

	// add worktree
	wtDir := filepath.Join(t.TempDir(), "worktree")
	if err := WorktreeAdd(ctx, bare, wtDir, sha); err != nil {
		t.Fatalf("WorktreeAdd failed: %v", err)
	}

	// verify worktree has the file
	content, err := os.ReadFile(filepath.Join(wtDir, "README.md"))
	if err != nil {
		t.Fatalf("worktree missing README.md: %v", err)
	}
	if string(content) != "# test\n" {
		t.Fatalf("unexpected content: %q", content)
	}

	// remove worktree
	if err := WorktreeRemove(ctx, bare, wtDir); err != nil {
		t.Fatalf("WorktreeRemove failed: %v", err)
	}

	// verify worktree directory is gone
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatal("worktree directory should not exist after removal")
	}
}

func TestFindMainRepoRoot(t *testing.T) {
	ctx := context.Background()
	mainRepo := initTestRepo(t)

	// For a normal repo, FindMainRepoRoot should return the same as FindGitRoot.
	mainRoot, err := FindMainRepoRoot(mainRepo)
	if err != nil {
		t.Fatalf("FindMainRepoRoot failed for main repo: %v", err)
	}
	expectedMain, _ := filepath.EvalSymlinks(mainRepo)
	gotMain, _ := filepath.EvalSymlinks(mainRoot)
	if gotMain != expectedMain {
		t.Fatalf("expected %q, got %q", expectedMain, gotMain)
	}

	// Create a worktree and verify FindMainRepoRoot returns the main repo root.
	run(t, mainRepo, "git", "checkout", "-b", "wt-branch")
	run(t, mainRepo, "git", "checkout", "-") // back to original branch
	wtDir := filepath.Join(t.TempDir(), "worktree")
	if err := WorktreeAdd(ctx, mainRepo, wtDir, "wt-branch"); err != nil {
		t.Fatalf("WorktreeAdd failed: %v", err)
	}
	t.Cleanup(func() { WorktreeRemove(ctx, mainRepo, wtDir) })

	// FindGitRoot from worktree returns the worktree path.
	wtRoot, err := FindGitRoot(wtDir)
	if err != nil {
		t.Fatalf("FindGitRoot from worktree failed: %v", err)
	}
	resolvedWt, _ := filepath.EvalSymlinks(wtDir)
	gotWt, _ := filepath.EvalSymlinks(wtRoot)
	if gotWt != resolvedWt {
		t.Fatalf("FindGitRoot should return worktree path %q, got %q", resolvedWt, gotWt)
	}

	// FindMainRepoRoot from worktree should return the main repo root.
	mainFromWt, err := FindMainRepoRoot(wtDir)
	if err != nil {
		t.Fatalf("FindMainRepoRoot from worktree failed: %v", err)
	}
	gotFromWt, _ := filepath.EvalSymlinks(mainFromWt)
	if gotFromWt != expectedMain {
		t.Fatalf("FindMainRepoRoot from worktree: expected %q, got %q", expectedMain, gotFromWt)
	}
}

func TestFindMainRepoRootNotFound(t *testing.T) {
	_, err := FindMainRepoRoot(t.TempDir())
	if err == nil {
		t.Fatal("expected error for non-git directory")
	}
}

func TestPush(t *testing.T) {
	ctx := context.Background()
	src := initTestRepo(t)
	bare := filepath.Join(t.TempDir(), "dest.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	run(t, src, "git", "remote", "add", "dest", bare)

	// push main branch
	if err := Push(ctx, src, "dest", "refs/heads/main", "", false); err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	// verify ref exists in bare repo
	out, err := Run(ctx, bare, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("ref not found in dest: %v", err)
	}
	expected := run(t, src, "git", "rev-parse", "HEAD")
	if out != expected {
		t.Fatalf("expected %q, got %q", expected, out)
	}
}

func TestPushForceWithLease(t *testing.T) {
	ctx := context.Background()
	src := initTestRepo(t)
	bare := filepath.Join(t.TempDir(), "dest.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	run(t, src, "git", "remote", "add", "dest", bare)
	run(t, src, "git", "push", "dest", "HEAD:refs/heads/main")

	expectedSHA := run(t, src, "git", "rev-parse", "HEAD")

	// make a new commit (simulating rebase)
	writeFile(t, filepath.Join(src, "new.txt"), "new\n")
	run(t, src, "git", "add", ".")
	run(t, src, "git", "commit", "-m", "new commit")

	// force-with-lease should succeed with correct expected SHA
	if err := Push(ctx, src, "dest", "refs/heads/main", expectedSHA, true); err != nil {
		t.Fatalf("PushForceWithLease failed: %v", err)
	}

	// verify new SHA
	out, err := Run(ctx, bare, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	newSHA := run(t, src, "git", "rev-parse", "HEAD")
	if out != newSHA {
		t.Fatalf("expected %q, got %q", newSHA, out)
	}
}

func TestLsRemote(t *testing.T) {
	ctx := context.Background()
	src := initTestRepo(t)
	bare := filepath.Join(t.TempDir(), "dest.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	run(t, src, "git", "remote", "add", "dest", bare)
	run(t, src, "git", "push", "dest", "HEAD:refs/heads/main")

	expectedSHA := run(t, src, "git", "rev-parse", "HEAD")

	// Query existing ref
	sha, err := LsRemote(ctx, src, bare, "refs/heads/main")
	if err != nil {
		t.Fatalf("LsRemote failed: %v", err)
	}
	if sha != expectedSHA {
		t.Fatalf("expected %q, got %q", expectedSHA, sha)
	}
}

func TestLsRemoteNotFound(t *testing.T) {
	ctx := context.Background()
	src := initTestRepo(t)
	bare := filepath.Join(t.TempDir(), "dest.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}

	// Query a ref that doesn't exist
	sha, err := LsRemote(ctx, src, bare, "refs/heads/nonexistent")
	if err != nil {
		t.Fatalf("LsRemote should not error for missing ref: %v", err)
	}
	if sha != "" {
		t.Fatalf("expected empty string for missing ref, got %q", sha)
	}
}

func TestDefaultBranch(t *testing.T) {
	ctx := context.Background()
	src := initTestRepo(t)
	bare := filepath.Join(t.TempDir(), "upstream.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	// Push to bare repo so HEAD ref exists.
	run(t, src, "git", "remote", "add", "upstream", bare)
	run(t, src, "git", "push", "upstream", "HEAD:refs/heads/main")

	branch := DefaultBranch(ctx, src, "upstream")
	if branch != "main" {
		t.Fatalf("expected 'main', got %q", branch)
	}
}

func TestDefaultBranchNonMain(t *testing.T) {
	ctx := context.Background()
	src := initTestRepo(t)
	bare := filepath.Join(t.TempDir(), "upstream.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	// Set bare repo HEAD to develop.
	run(t, bare, "git", "symbolic-ref", "HEAD", "refs/heads/develop")
	// Push a develop branch.
	run(t, src, "git", "remote", "add", "upstream", bare)
	run(t, src, "git", "push", "upstream", "HEAD:refs/heads/develop")

	branch := DefaultBranch(ctx, src, "upstream")
	if branch != "develop" {
		t.Fatalf("expected 'develop', got %q", branch)
	}
}

func TestDefaultBranchFallback(t *testing.T) {
	ctx := context.Background()
	src := initTestRepo(t)
	// Remote doesn't exist — should fall back to "main".
	branch := DefaultBranch(ctx, src, "nonexistent")
	if branch != "main" {
		t.Fatalf("expected 'main' fallback, got %q", branch)
	}
}

func TestDefaultBranchEmptyRemote(t *testing.T) {
	ctx := context.Background()
	src := initTestRepo(t)
	bare := filepath.Join(t.TempDir(), "empty.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	// Empty bare repo (no refs) — HEAD symref exists but target doesn't.
	// ls-remote returns nothing for HEAD, so should fall back to "main".
	run(t, src, "git", "remote", "add", "empty", bare)
	branch := DefaultBranch(ctx, src, "empty")
	if branch != "main" {
		t.Fatalf("expected 'main' fallback for empty remote, got %q", branch)
	}
}

func TestCurrentBranch(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	// default branch after init — could be main or master depending on git config
	branch, err := CurrentBranch(ctx, dir)
	if err != nil {
		t.Fatalf("CurrentBranch failed: %v", err)
	}
	if branch == "" {
		t.Fatal("expected non-empty branch name")
	}

	// create and checkout a new branch
	run(t, dir, "git", "checkout", "-b", "feature")
	branch, err = CurrentBranch(ctx, dir)
	if err != nil {
		t.Fatalf("CurrentBranch on feature failed: %v", err)
	}
	if branch != "feature" {
		t.Fatalf("expected 'feature', got %q", branch)
	}
}
