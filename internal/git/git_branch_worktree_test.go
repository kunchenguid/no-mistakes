package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// addSubmodule wires up an absorbed submodule at <super>/<name> pointing at
// the repo at <remoteDir>, commits the new submodule, and returns the
// submodule's working tree. The path layout matches `git submodule add`:
//   - <super>/<name>                        working tree
//   - <super>/.git/modules/<name>           absorbed git dir
//   - core.worktree = ../../../../<name>    relative inside the absorbed git dir
func addSubmodule(t *testing.T, superDir, name, remoteDir string) string {
	t.Helper()
	// Use an absolute path so the clone does not depend on the superproject
	// having an "origin" remote (which would change the relative path's
	// resolution base).
	absRemote, err := filepath.Abs(remoteDir)
	if err != nil {
		t.Fatalf("abs remote: %v", err)
	}
	// superDir may itself be a submodule checkout (nested case), which never
	// had a commit identity configured; set one so the commit below does not
	// fail with "empty ident name" on runners without an ambient git identity.
	run(t, superDir, "git", "config", "user.email", "test@test.com")
	run(t, superDir, "git", "config", "user.name", "Test")
	run(t, superDir, "git", "-c", "protocol.file.allow=always", "submodule", "add", absRemote, name)
	run(t, superDir, "git", "-c", "protocol.file.allow=always", "submodule", "init", name)
	run(t, superDir, "git", "-c", "protocol.file.allow=always", "submodule", "update", name)
	run(t, superDir, "git", "add", ".gitmodules", name)
	run(t, superDir, "git", "commit", "-m", "add "+name+" submodule")
	return filepath.Join(superDir, name)
}

// TestFindMainRepoRootSubmodule reproduces issue #328: in an absorbed
// submodule checkout, the git common dir is <super>/.git/modules/<name>, so
// the historical "parent of commonDir" heuristic points at the superproject's
// modules directory. The function must instead resolve to the submodule's
// own working tree (via core.worktree) so callers read the submodule's
// remotes, not the superproject's.
func TestFindMainRepoRootSubmodule(t *testing.T) {
	ctx := context.Background()
	superDir := initTestRepo(t)
	// Give the superproject its own distinct origin so the assertion below
	// can tell the two apart — the bug is that the gate ends up reading
	// the superproject's origin instead of the submodule's.
	if err := AddRemote(ctx, superDir, "origin", "https://github.com/example/superproject.git"); err != nil {
		t.Fatalf("add super origin: %v", err)
	}
	subRemote := initTestRepo(t)

	subWorktree := addSubmodule(t, superDir, "sub", subRemote)
	// Set the submodule's own origin on its working tree (the helper above
	// clones subRemote, so without this the cloned submodule's "origin"
	// would be subRemote's path, not the URL the gate should record).
	// `git submodule add` already created an "origin" remote pointing at
	// subRemote, so use EnsureRemote to overwrite it idempotently.
	if err := EnsureRemote(ctx, subWorktree, "origin", "https://github.com/example/submodule.git"); err != nil {
		t.Fatalf("add submodule origin: %v", err)
	}

	got, err := FindMainRepoRoot(subWorktree)
	if err != nil {
		t.Fatalf("FindMainRepoRoot from submodule: %v", err)
	}
	wantResolved, _ := filepath.EvalSymlinks(subWorktree)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Fatalf("FindMainRepoRoot from submodule = %q, want %q (superproject's .git/modules would be wrong)", gotResolved, wantResolved)
	}

	// The dangerous half: callers (init, eject) read remotes by running git
	// with cwd set to the returned root. If that root resolves to the
	// superproject instead, the gate records the superproject's origin
	// instead of the submodule's.
	url, err := Run(ctx, got, "remote", "get-url", "origin")
	if err != nil {
		t.Fatalf("read origin from returned root: %v", err)
	}
	if url != "https://github.com/example/submodule.git" {
		t.Fatalf("origin read from returned root = %q, want the submodule's origin", url)
	}
}

// TestFindMainRepoRootNestedSubmodule is the same fix one level deeper:
// a submodule inside a submodule has its git dir at
// <super>/.git/modules/a/modules/b. The function must still resolve to the
// innermost submodule's working tree.
func TestFindMainRepoRootNestedSubmodule(t *testing.T) {
	ctx := context.Background()
	superDir := initTestRepo(t)
	outerRemote := initTestRepo(t)
	innerRemote := initTestRepo(t)

	outerWorktree := addSubmodule(t, superDir, "outer", outerRemote)
	innerWorktree := addSubmodule(t, outerWorktree, "inner", innerRemote)
	if err := AddRemote(ctx, innerRemote, "origin", "https://github.com/example/inner.git"); err != nil {
		t.Fatalf("add inner origin: %v", err)
	}

	got, err := FindMainRepoRoot(innerWorktree)
	if err != nil {
		t.Fatalf("FindMainRepoRoot from nested submodule: %v", err)
	}
	wantResolved, _ := filepath.EvalSymlinks(innerWorktree)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Fatalf("FindMainRepoRoot from nested submodule = %q, want %q", gotResolved, wantResolved)
	}
}

// TestFindMainRepoRootFromWorktreeStillResolvesToMain is the linked-worktree
// regression guard for the three-way fix. Linked worktrees keep their git
// common dir at <main>/.git, so the historical "parent of commonDir" answer
// stays correct and must not regress.
func TestFindMainRepoRootFromWorktreeStillResolvesToMain(t *testing.T) {
	ctx := context.Background()
	mainRepo := initTestRepo(t)
	run(t, mainRepo, "git", "checkout", "-b", "wt-branch")
	run(t, mainRepo, "git", "checkout", "-")
	wtDir := filepath.Join(t.TempDir(), "worktree")
	if err := WorktreeAdd(ctx, mainRepo, wtDir, "wt-branch"); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	t.Cleanup(func() { WorktreeRemove(ctx, mainRepo, wtDir) })

	got, err := FindMainRepoRoot(wtDir)
	if err != nil {
		t.Fatalf("FindMainRepoRoot from worktree: %v", err)
	}
	want, _ := filepath.EvalSymlinks(mainRepo)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != want {
		t.Fatalf("FindMainRepoRoot from worktree = %q, want %q", gotResolved, want)
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

func TestPushWithOptionsForwardsPushOptions(t *testing.T) {
	ctx := context.Background()
	src := initTestRepo(t)
	bare := filepath.Join(t.TempDir(), "dest.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	run(t, bare, "git", "config", "receive.advertisePushOptions", "true")
	run(t, src, "git", "remote", "add", "dest", bare)

	marker := filepath.Join(t.TempDir(), "push-options.txt")
	hook := "#!/bin/sh\nprintf '%s:%s\n' \"$GIT_PUSH_OPTION_COUNT\" \"$GIT_PUSH_OPTION_0\" > " + shellSingleQuote(marker) + "\n"
	if err := os.WriteFile(filepath.Join(bare, "hooks", "post-receive"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := PushWithOptions(ctx, src, "dest", "refs/heads/main", "", false, []string{"no-mistakes.skip=test,lint"}); err != nil {
		t.Fatalf("PushWithOptions failed: %v", err)
	}

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(data)), "1:no-mistakes.skip=test,lint"; got != want {
		t.Fatalf("push options marker = %q, want %q", got, want)
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
