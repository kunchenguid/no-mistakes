package git

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "no-mistakes-git-tests-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "gitconfig")); err != nil {
		panic(err)
	}
	if err := os.Setenv("GIT_CONFIG_NOSYSTEM", "1"); err != nil {
		panic(err)
	}
	// Agent harnesses inject git config (e.g. safe.bareRepository=explicit)
	// via GIT_CONFIG_COUNT/KEY_n/VALUE_n; tests that need it re-set it with
	// t.Setenv (issue #362).
	if err := os.Unsetenv("GIT_CONFIG_COUNT"); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

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

func gitBlobSHA1(content string) string {
	blob := sha1.Sum([]byte("blob " + strconv.Itoa(len(content)) + "\x00" + content))
	return hex.EncodeToString(blob[:])
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

func TestCopyLocalUserIdentity(t *testing.T) {
	ctx := context.Background()
	src := initTestRepo(t)
	dst := initTestRepo(t)

	run(t, dst, "git", "config", "--local", "--unset", "user.name")
	run(t, dst, "git", "config", "--local", "--unset", "user.email")

	if err := CopyLocalUserIdentity(ctx, src, dst); err != nil {
		t.Fatalf("CopyLocalUserIdentity failed: %v", err)
	}

	if got := run(t, dst, "git", "config", "--local", "--get", "user.name"); got != "Test" {
		t.Fatalf("user.name = %q, want %q", got, "Test")
	}
	if got := run(t, dst, "git", "config", "--local", "--get", "user.email"); got != "test@test.com" {
		t.Fatalf("user.email = %q, want %q", got, "test@test.com")
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

func TestHasUncommittedChangesClean(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	dirty, err := HasUncommittedChanges(ctx, dir)
	if err != nil {
		t.Fatalf("HasUncommittedChanges failed: %v", err)
	}
	if dirty {
		t.Fatal("expected clean repo, got dirty")
	}
}

func TestHasUncommittedChangesModifiedFile(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	writeFile(t, filepath.Join(dir, "README.md"), "# changed\n")

	dirty, err := HasUncommittedChanges(ctx, dir)
	if err != nil {
		t.Fatalf("HasUncommittedChanges failed: %v", err)
	}
	if !dirty {
		t.Fatal("expected dirty repo after modifying file")
	}
}

func TestHasUncommittedChangesUntrackedFile(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	writeFile(t, filepath.Join(dir, "new.txt"), "new\n")

	dirty, err := HasUncommittedChanges(ctx, dir)
	if err != nil {
		t.Fatalf("HasUncommittedChanges failed: %v", err)
	}
	if !dirty {
		t.Fatal("expected dirty repo with untracked file")
	}
}

func TestHasUncommittedChangesStagedOnly(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	writeFile(t, filepath.Join(dir, "staged.txt"), "staged\n")
	run(t, dir, "git", "add", "staged.txt")

	dirty, err := HasUncommittedChanges(ctx, dir)
	if err != nil {
		t.Fatalf("HasUncommittedChanges failed: %v", err)
	}
	if !dirty {
		t.Fatal("expected dirty repo with staged-only change")
	}
}

func TestCreateBranch(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	if err := CreateBranch(ctx, dir, "feature/new"); err != nil {
		t.Fatalf("CreateBranch failed: %v", err)
	}

	branch, err := CurrentBranch(ctx, dir)
	if err != nil {
		t.Fatalf("CurrentBranch failed: %v", err)
	}
	if branch != "feature/new" {
		t.Fatalf("expected 'feature/new', got %q", branch)
	}
}

func TestCreateBranchDuplicate(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	if err := CreateBranch(ctx, dir, "dup"); err != nil {
		t.Fatalf("first CreateBranch failed: %v", err)
	}
	// Switch away so we can try to create the same branch again.
	run(t, dir, "git", "checkout", "-")

	if err := CreateBranch(ctx, dir, "dup"); err == nil {
		t.Fatal("expected error creating duplicate branch")
	}
}

func TestCommitAll(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	writeFile(t, filepath.Join(dir, "a.txt"), "a\n")
	writeFile(t, filepath.Join(dir, "b.txt"), "b\n")

	if err := CommitAll(ctx, dir, "add a and b"); err != nil {
		t.Fatalf("CommitAll failed: %v", err)
	}

	dirty, err := HasUncommittedChanges(ctx, dir)
	if err != nil {
		t.Fatalf("HasUncommittedChanges after commit failed: %v", err)
	}
	if dirty {
		t.Fatal("expected clean repo after CommitAll")
	}

	msg := run(t, dir, "git", "log", "-1", "--pretty=%B")
	if !strings.Contains(msg, "add a and b") {
		t.Fatalf("commit message missing subject, got %q", msg)
	}
}

func TestCommitAllNoChanges(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	if err := CommitAll(ctx, dir, "nothing"); err == nil {
		t.Fatal("expected error committing with no changes")
	}
}

func TestIsLocalToolchainPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{".tools/dotnet-cli-home/NuGet/foo.nupkg", true},
		{"src/.tools/cache", true},
		{"node_modules/left-pad/index.js", true},
		{"pkg/__pycache__/x.pyc", true},
		{".yarn/unplugged/pkg/index.js", true},
		{".yarn/install-state.gz", true},
		{".yarn/cache/lodash-npm-1.0.0.zip", false},
		{".yarn/releases/yarn-4.0.0.cjs", false},
		{"internal/cli/status.go", false},
		{"tools/scripts/setup.sh", false}, // "tools" without leading dot is source
		{"README.md", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsLocalToolchainPath(tt.path); got != tt.want {
			t.Errorf("IsLocalToolchainPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestStageAll_ExcludesDotnetCLIHomeCache(t *testing.T) {
	// Regression: pipeline lint/format runs that set DOTNET_CLI_HOME to a
	// worktree-relative path (e.g. .tools/dotnet-cli-home) must not ship the
	// NuGet/tool cache into a gate commit via git add -A.
	dir := initTestRepo(t)
	ctx := context.Background()

	writeFile(t, filepath.Join(dir, "Program.cs"), "class Program {}\n")
	cacheDir := filepath.Join(dir, ".tools", "dotnet-cli-home", "NuGet", "packages")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cacheContents := "cache blob must not enter the object database\n"
	writeFile(t, filepath.Join(cacheDir, "sentinel.nupkg"), cacheContents)
	// Also drop a node_modules tree the way npm would.
	nm := filepath.Join(dir, "node_modules", "left-pad")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(nm, "index.js"), "module.exports = 1\n")

	excluded, err := StageAll(ctx, dir)
	if err != nil {
		t.Fatalf("StageAll: %v", err)
	}
	if len(excluded) == 0 {
		t.Fatal("expected toolchain paths to be excluded")
	}
	for _, p := range excluded {
		if !IsLocalToolchainPath(p) {
			t.Errorf("excluded non-toolchain path %q", p)
		}
	}

	staged, err := HasStagedChanges(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !staged {
		t.Fatal("expected Program.cs to remain staged")
	}
	cached := run(t, dir, "git", "diff", "--cached", "--name-only")
	if !strings.Contains(cached, "Program.cs") {
		t.Fatalf("Program.cs missing from index:\n%s", cached)
	}
	if strings.Contains(cached, ".tools") || strings.Contains(cached, "node_modules") {
		t.Fatalf("toolchain paths still staged:\n%s", cached)
	}
	if _, err := Run(ctx, dir, "cat-file", "-e", gitBlobSHA1(cacheContents)+"^{blob}"); err == nil {
		t.Fatal("excluded cache blob was written to the object database")
	}

	if err := CommitAll(ctx, dir, "format Program.cs"); err != nil {
		// StageAll already ran; CommitAll will StageAll again which is fine.
		// But index may already be staged - CommitAll does StageAll then commit.
		t.Fatalf("CommitAll: %v", err)
	}
	// Ensure committed tree has no .tools
	tree := run(t, dir, "git", "ls-tree", "-r", "--name-only", "HEAD")
	if strings.Contains(tree, ".tools") || strings.Contains(tree, "node_modules") {
		t.Fatalf("commit contains toolchain paths:\n%s", tree)
	}
	if !strings.Contains(tree, "Program.cs") {
		t.Fatalf("commit missing Program.cs:\n%s", tree)
	}
}

func TestStageAll_DoesNotStoreLocalYarnState(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(dir, ".yarn"), 0o755); err != nil {
		t.Fatal(err)
	}
	contents := "local Yarn state must not enter the object database\n"
	writeFile(t, filepath.Join(dir, ".yarn", "install-state.gz"), contents)

	if _, err := StageAll(ctx, dir); err != nil {
		t.Fatalf("StageAll: %v", err)
	}
	if _, err := Run(ctx, dir, "cat-file", "-e", gitBlobSHA1(contents)+"^{blob}"); err == nil {
		t.Fatal("excluded Yarn state blob was written to the object database")
	}
}

func TestStageAll_PreservesYarnZeroInstallFiles(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	writeFile(t, filepath.Join(dir, "app.js"), "console.log('ok')\n")
	if err := os.MkdirAll(filepath.Join(dir, ".yarn", "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".yarn", "releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".yarn", "unplugged", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".yarn", "cache", "app-npm-1.0.0.zip"), "package cache\n")
	writeFile(t, filepath.Join(dir, ".yarn", "releases", "yarn-4.0.0.cjs"), "release\n")
	writeFile(t, filepath.Join(dir, ".yarn", "unplugged", "app", "build.js"), "local build output\n")

	if _, err := StageAll(ctx, dir); err != nil {
		t.Fatalf("StageAll: %v", err)
	}
	cached := run(t, dir, "git", "diff", "--cached", "--name-only")
	for _, path := range []string{"app.js", ".yarn/cache/app-npm-1.0.0.zip", ".yarn/releases/yarn-4.0.0.cjs"} {
		if !strings.Contains(cached, path) {
			t.Errorf("expected %q staged:\n%s", path, cached)
		}
	}
	if strings.Contains(cached, ".yarn/unplugged/") {
		t.Fatalf("local Yarn unplugged files were staged:\n%s", cached)
	}
}

func TestCommitAll_OnlyToolchainJunkIsNoOpError(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	cacheDir := filepath.Join(dir, ".tools", "dotnet-cli-home")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(cacheDir, "x"), "cache\n")

	if err := CommitAll(ctx, dir, "should not commit junk"); err == nil {
		t.Fatal("expected error when only toolchain junk is present")
	}
	// Cache must remain on disk (untracked), not deleted.
	if _, err := os.Stat(filepath.Join(cacheDir, "x")); err != nil {
		t.Fatalf("toolchain file should remain on disk: %v", err)
	}
}

func TestIsDetachedHEADOnBranch(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	detached, err := IsDetachedHEAD(ctx, dir)
	if err != nil {
		t.Fatalf("IsDetachedHEAD failed: %v", err)
	}
	if detached {
		t.Fatal("fresh repo on a branch should not be detached")
	}
}

func TestIsDetachedHEADWhenDetached(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	// Make a second commit so we have a specific SHA to detach onto.
	writeFile(t, filepath.Join(dir, "two.txt"), "two\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "second")
	sha := run(t, dir, "git", "rev-parse", "HEAD~1")

	run(t, dir, "git", "checkout", sha)

	detached, err := IsDetachedHEAD(ctx, dir)
	if err != nil {
		t.Fatalf("IsDetachedHEAD failed: %v", err)
	}
	if !detached {
		t.Fatal("expected detached HEAD after checking out a commit SHA")
	}
}

// setSafeBareRepositoryExplicit injects the git config used by agent
// harnesses (e.g. Claude Code) and hardened CI environments, which forbids
// cwd-based discovery of bare repositories (issue #362).
func setSafeBareRepositoryExplicit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "explicit")
}

func TestRunOnBareRepoUnderSafeBareRepositoryExplicit(t *testing.T) {
	setSafeBareRepositoryExplicit(t)
	ctx := context.Background()

	bare := filepath.Join(t.TempDir(), "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatalf("init bare: %v", err)
	}

	if _, err := Run(ctx, bare, "config", "receive.advertisePushOptions", "true"); err != nil {
		t.Fatalf("config write on bare repo: %v", err)
	}
	got, err := Run(ctx, bare, "config", "--get", "receive.advertisePushOptions")
	if err != nil {
		t.Fatalf("config read on bare repo: %v", err)
	}
	if got != "true" {
		t.Fatalf("receive.advertisePushOptions = %q, want true", got)
	}

	// A working repo must keep using normal cwd discovery.
	work := initTestRepo(t)
	if out, err := Run(ctx, work, "rev-parse", "--is-inside-work-tree"); err != nil || out != "true" {
		t.Fatalf("rev-parse in working repo = %q, %v; want true, nil", out, err)
	}
}

func TestWorktreeAddRemoveOnBareRepoUnderSafeBareRepositoryExplicit(t *testing.T) {
	setSafeBareRepositoryExplicit(t)
	ctx := context.Background()

	work := initTestRepo(t)
	bare := filepath.Join(t.TempDir(), "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	run(t, work, "git", "push", bare, "HEAD:refs/heads/main")
	sha := run(t, work, "git", "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "wt")
	if err := WorktreeAdd(ctx, bare, wt, sha); err != nil {
		t.Fatalf("worktree add from bare repo: %v", err)
	}
	if got, err := Run(ctx, wt, "rev-parse", "HEAD"); err != nil || got != sha {
		t.Fatalf("rev-parse in worktree = %q, %v; want %q, nil", got, err, sha)
	}
	if err := WorktreeRemove(ctx, bare, wt); err != nil {
		t.Fatalf("worktree remove from bare repo: %v", err)
	}
}
