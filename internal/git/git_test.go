package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// helper: create a non-bare git repo with an initial commit
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init")
	run(t, dir, "git", "config", "user.email", "test@test.com")
	run(t, dir, "git", "config", "user.name", "Test")
	run(t, dir, "git", "config", "core.autocrlf", "false")
	writeFile(t, filepath.Join(dir, "README.md"), "# test\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "initial")
	return dir
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRun(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	out, err := Run(ctx, dir, "status", "--porcelain")
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if out != "" {
		t.Fatalf("expected clean status, got: %q", out)
	}
}

func TestRunError(t *testing.T) {
	ctx := context.Background()
	_, err := Run(ctx, t.TempDir(), "log")
	if err == nil {
		t.Fatal("expected error for git log in non-repo")
	}
	// error should contain stderr info
	if !strings.Contains(err.Error(), "git log") {
		t.Fatalf("expected error to mention command, got: %v", err)
	}
}

func TestInitBare(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "test.git")

	if err := InitBare(ctx, dir); err != nil {
		t.Fatalf("InitBare failed: %v", err)
	}

	// verify it's a bare repo
	out, err := Run(ctx, dir, "rev-parse", "--is-bare-repository")
	if err != nil {
		t.Fatalf("rev-parse failed: %v", err)
	}
	if out != "true" {
		t.Fatalf("expected bare repo, got: %q", out)
	}
}

func TestAddRemoteAndGetURL(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	if err := AddRemote(ctx, dir, "upstream", "https://github.com/test/repo.git"); err != nil {
		t.Fatalf("AddRemote failed: %v", err)
	}

	url, err := GetRemoteURL(ctx, dir, "upstream")
	if err != nil {
		t.Fatalf("GetRemoteURL failed: %v", err)
	}
	if url != "https://github.com/test/repo.git" {
		t.Fatalf("expected url, got: %q", url)
	}
}

func TestRemoveRemote(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	_ = AddRemote(ctx, dir, "upstream", "https://github.com/test/repo.git")
	if err := RemoveRemote(ctx, dir, "upstream"); err != nil {
		t.Fatalf("RemoveRemote failed: %v", err)
	}

	_, err := GetRemoteURL(ctx, dir, "upstream")
	if err == nil {
		t.Fatal("expected error after removing remote")
	}
}

func TestGetRemoteURLNotFound(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	_, err := GetRemoteURL(ctx, dir, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent remote")
	}
}

func TestFindGitRoot(t *testing.T) {
	dir := initTestRepo(t)

	// from repo root
	root, err := FindGitRoot(dir)
	if err != nil {
		t.Fatalf("FindGitRoot failed: %v", err)
	}
	// resolve symlinks for comparison (macOS /private/var/...)
	expected, _ := filepath.EvalSymlinks(dir)
	got, _ := filepath.EvalSymlinks(root)
	if got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}

	// from subdirectory
	sub := filepath.Join(dir, "a", "b", "c")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err = FindGitRoot(sub)
	if err != nil {
		t.Fatalf("FindGitRoot from subdir failed: %v", err)
	}
	got, _ = filepath.EvalSymlinks(root)
	if got != expected {
		t.Fatalf("expected %q from subdir, got %q", expected, got)
	}
}

func TestFindGitRootNotFound(t *testing.T) {
	_, err := FindGitRoot(t.TempDir())
	if err == nil {
		t.Fatal("expected error for non-git directory")
	}
}

func TestDiff(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	// get initial commit SHA
	base := run(t, dir, "git", "rev-parse", "HEAD")

	// make a change and commit
	writeFile(t, filepath.Join(dir, "file.txt"), "hello\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "add file")
	head := run(t, dir, "git", "rev-parse", "HEAD")

	diff, err := Diff(ctx, dir, base, head)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	if !strings.Contains(diff, "file.txt") {
		t.Fatalf("diff should mention file.txt, got: %q", diff)
	}
	if !strings.Contains(diff, "+hello") {
		t.Fatalf("diff should contain +hello, got: %q", diff)
	}
}

func TestDiffEmpty(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()
	head := run(t, dir, "git", "rev-parse", "HEAD")

	diff, err := Diff(ctx, dir, head, head)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}
	if diff != "" {
		t.Fatalf("expected empty diff, got: %q", diff)
	}
}

func TestDiffAgainstEmptyTree(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()
	head := run(t, dir, "git", "rev-parse", "HEAD")
	const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

	diff, err := Diff(ctx, dir, emptyTreeSHA, head)
	if err != nil {
		t.Fatalf("Diff against empty tree failed: %v", err)
	}
	if !strings.Contains(diff, "README.md") {
		t.Fatalf("diff should mention README.md, got: %q", diff)
	}
	if !strings.Contains(diff, "+# test") {
		t.Fatalf("diff should include initial file contents, got: %q", diff)
	}
}

func TestDiffHead(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	// No changes — should be empty
	diff, err := DiffHead(ctx, dir)
	if err != nil {
		t.Fatalf("DiffHead (clean) failed: %v", err)
	}
	if diff != "" {
		t.Fatalf("expected empty diff for clean tree, got: %q", diff)
	}

	// Unstaged changes
	writeFile(t, filepath.Join(dir, "new.txt"), "content\n")
	run(t, dir, "git", "add", "new.txt")
	writeFile(t, filepath.Join(dir, "new.txt"), "modified\n")

	diff, err = DiffHead(ctx, dir)
	if err != nil {
		t.Fatalf("DiffHead (unstaged) failed: %v", err)
	}
	if !strings.Contains(diff, "new.txt") {
		t.Fatalf("diff should mention new.txt, got: %q", diff)
	}
	if !strings.Contains(diff, "+modified") {
		t.Fatalf("diff should contain +modified, got: %q", diff)
	}
}

func TestDiffHead_StagedChanges(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	// Staged but uncommitted changes
	writeFile(t, filepath.Join(dir, "staged.txt"), "staged content\n")
	run(t, dir, "git", "add", "staged.txt")

	diff, err := DiffHead(ctx, dir)
	if err != nil {
		t.Fatalf("DiffHead (staged) failed: %v", err)
	}
	if !strings.Contains(diff, "staged.txt") {
		t.Fatalf("diff should mention staged.txt, got: %q", diff)
	}
	if !strings.Contains(diff, "+staged content") {
		t.Fatalf("diff should contain +staged content, got: %q", diff)
	}
}

func TestLog(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	base := run(t, dir, "git", "rev-parse", "HEAD")

	writeFile(t, filepath.Join(dir, "a.txt"), "a\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "first change")

	writeFile(t, filepath.Join(dir, "b.txt"), "b\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "second change")

	head := run(t, dir, "git", "rev-parse", "HEAD")

	log, err := Log(ctx, dir, base, head)
	if err != nil {
		t.Fatalf("Log failed: %v", err)
	}
	if !strings.Contains(log, "first change") {
		t.Fatalf("log should contain first change, got: %q", log)
	}
	if !strings.Contains(log, "second change") {
		t.Fatalf("log should contain second change, got: %q", log)
	}
}

func TestLogAgainstEmptyTree(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()
	head := run(t, dir, "git", "rev-parse", "HEAD")
	const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

	log, err := Log(ctx, dir, emptyTreeSHA, head)
	if err != nil {
		t.Fatalf("Log against empty tree failed: %v", err)
	}
	if !strings.Contains(log, "initial") {
		t.Fatalf("log should contain initial commit, got: %q", log)
	}
}

func TestHeadSHA(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	sha, err := HeadSHA(ctx, dir)
	if err != nil {
		t.Fatalf("HeadSHA failed: %v", err)
	}
	if len(sha) != 40 {
		t.Fatalf("expected 40-char SHA, got %d chars: %q", len(sha), sha)
	}
}

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
