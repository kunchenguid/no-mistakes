package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
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

func TestRunAppliesContextEnvironmentToGitSubprocesses(t *testing.T) {
	dir := initTestRepo(t)
	run(t, dir, "git", "config", "alias.show-forge", "!printf 'config:%s token:%s' \"$GH_CONFIG_DIR\" \"${GH_TOKEN:+set}\"")
	t.Setenv("GH_TOKEN", "ambient-must-not-leak")
	ctx := WithEnvironment(context.Background(), runenv.Overlay{
		Set:   map[string]string{"GH_CONFIG_DIR": "/profiles/personal"},
		Unset: []string{"GH_TOKEN"},
	})

	out, err := Run(ctx, dir, "show-forge")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "config:/profiles/personal token:" {
		t.Fatalf("git subprocess environment = %q", out)
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

func TestBindUserIdentity(t *testing.T) {
	ctx := context.Background()
	src := initTestRepo(t)
	dst := initTestRepo(t)

	run(t, dst, "git", "config", "--local", "--unset", "user.name")
	run(t, dst, "git", "config", "--local", "--unset", "user.email")

	if err := BindUserIdentity(ctx, src, dst); err != nil {
		t.Fatalf("BindUserIdentity failed: %v", err)
	}

	if got := run(t, dst, "git", "config", "--local", "--get", "user.name"); got != "Test" {
		t.Fatalf("user.name = %q, want %q", got, "Test")
	}
	if got := run(t, dst, "git", "config", "--local", "--get", "user.email"); got != "test@test.com" {
		t.Fatalf("user.email = %q, want %q", got, "test@test.com")
	}
}

// The checkout's effective identity may live anywhere in its config stack -
// the common case is a global file, which repository-local reads miss. A bare
// gate clone has no config of its own, so only propagating the effective
// identity keeps pipeline commits authored by the same party as the
// operator's own commits (issue #924).
func TestBindUserIdentityPropagatesEffectiveIdentity(t *testing.T) {
	ctx := context.Background()

	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalConfig, []byte("[user]\n\tname = Global User\n\temail = global@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)

	parent := t.TempDir()
	src := filepath.Join(parent, "src")
	dst := filepath.Join(parent, "dst")
	run(t, parent, "git", "init", src)
	run(t, parent, "git", "init", dst)

	if err := BindUserIdentity(ctx, src, dst); err != nil {
		t.Fatalf("BindUserIdentity failed: %v", err)
	}
	if got := run(t, dst, "git", "config", "--local", "--get", "user.name"); got != "Global User" {
		t.Fatalf("user.name = %q, want %q", got, "Global User")
	}
	if got := run(t, dst, "git", "config", "--local", "--get", "user.email"); got != "global@example.com" {
		t.Fatalf("user.email = %q, want %q", got, "global@example.com")
	}
}

// A checkout that resolves no identity anywhere leaves the worktree to the
// daemon host's ambient or auto-detected identity. BindUserIdentity refuses
// rather than letting a commit be published under it.
func TestBindUserIdentityRefusesUnboundWorktree(t *testing.T) {
	ctx := context.Background()

	parent := t.TempDir()
	src := filepath.Join(parent, "src")
	dst := filepath.Join(parent, "dst")
	run(t, parent, "git", "init", src)
	run(t, parent, "git", "init", dst)

	for _, k := range []string{
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	} {
		if v, ok := os.LookupEnv(k); ok {
			os.Unsetenv(k)
			t.Cleanup(func() { os.Setenv(k, v) })
		}
	}

	err := BindUserIdentity(ctx, src, dst)
	if err == nil {
		t.Fatal("expected BindUserIdentity to refuse an unbound worktree")
	}
	if !strings.Contains(err.Error(), "cannot bind a git identity") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// GIT_AUTHOR_*/GIT_COMMITTER_* in the daemon's environment are an explicit
// binding and must keep their precedence: nothing is stamped, nothing fails.
func TestBindUserIdentityEnvironmentBinds(t *testing.T) {
	ctx := context.Background()

	parent := t.TempDir()
	src := filepath.Join(parent, "src")
	dst := filepath.Join(parent, "dst")
	run(t, parent, "git", "init", src)
	run(t, parent, "git", "init", dst)

	t.Setenv("GIT_AUTHOR_NAME", "Env User")
	t.Setenv("GIT_AUTHOR_EMAIL", "env-author@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Env User")
	t.Setenv("GIT_COMMITTER_EMAIL", "env-committer@example.com")

	if err := BindUserIdentity(ctx, src, dst); err != nil {
		t.Fatalf("BindUserIdentity failed: %v", err)
	}
	if got := run(t, dst, "git", "var", "GIT_AUTHOR_IDENT"); !strings.HasPrefix(got, "Env User <env-author@example.com>") {
		t.Fatalf("author ident = %q, want env-bound identity", got)
	}
}

func TestBindUserIdentityPropagatesRoleSpecificIdentity(t *testing.T) {
	ctx := context.Background()

	parent := t.TempDir()
	src := filepath.Join(parent, "src")
	dst := filepath.Join(parent, "dst")
	run(t, parent, "git", "init", src)
	run(t, parent, "git", "init", dst)

	run(t, src, "git", "config", "user.name", "Generic User")
	run(t, src, "git", "config", "user.email", "generic@example.com")
	run(t, src, "git", "config", "author.name", "Author User")
	run(t, src, "git", "config", "author.email", "author@example.com")

	for _, k := range []string{
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	} {
		if v, ok := os.LookupEnv(k); ok {
			os.Unsetenv(k)
			t.Cleanup(func() { os.Setenv(k, v) })
		}
	}

	if err := BindUserIdentity(ctx, src, dst); err != nil {
		t.Fatalf("BindUserIdentity failed: %v", err)
	}
	if got := run(t, dst, "git", "var", "GIT_AUTHOR_IDENT"); !strings.HasPrefix(got, "Author User <author@example.com>") {
		t.Fatalf("author ident = %q, want role-specific author identity", got)
	}
	if got := run(t, dst, "git", "var", "GIT_COMMITTER_IDENT"); !strings.HasPrefix(got, "Generic User <generic@example.com>") {
		t.Fatalf("committer ident = %q, want generic committer identity", got)
	}
}

func TestBindUserIdentityRefusesStaleSharedIdentityOnLocalFallback(t *testing.T) {
	ctx := context.Background()

	parent := t.TempDir()
	src := filepath.Join(parent, "src")
	run(t, parent, "git", "init", src)

	// dst is a linked worktree of a gate without extensions.worktreeConfig,
	// so identity config falls back to the gate's shared local config - which
	// an earlier run has already stamped with a different identity, including
	// role keys this checkout does not set.
	gate := filepath.Join(parent, "gate")
	run(t, parent, "git", "init", gate)
	run(t, gate, "git", "config", "user.name", "Stale User")
	run(t, gate, "git", "config", "user.email", "stale@example.com")
	run(t, gate, "git", "config", "author.name", "Stale Author")
	run(t, gate, "git", "config", "author.email", "stale-author@example.com")
	writeFile(t, filepath.Join(gate, "README.md"), "# gate\n")
	run(t, gate, "git", "add", ".")
	run(t, gate, "git", "commit", "-m", "initial")
	dst := filepath.Join(parent, "wt")
	run(t, gate, "git", "worktree", "add", "--detach", dst)

	run(t, src, "git", "config", "user.name", "Current User")
	run(t, src, "git", "config", "user.email", "current@example.com")

	for _, k := range []string{
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	} {
		if v, ok := os.LookupEnv(k); ok {
			os.Unsetenv(k)
			t.Cleanup(func() { os.Setenv(k, v) })
		}
	}

	err := BindUserIdentity(ctx, src, dst)
	if err == nil {
		t.Fatal("BindUserIdentity succeeded despite a stale shared-config identity")
	}
	if !strings.Contains(err.Error(), "cannot bind a git identity") {
		t.Fatalf("error = %q, want bind failure", err)
	}
	// The stamp writes our values over the stale ones (legacy fallback
	// behavior), but the bind attempt must never clear keys an active run
	// may be using.
	if got := run(t, dst, "git", "config", "--local", "--get", "--default", "", "user.name"); got != "Current User" {
		t.Fatalf("user.name = %q, want stamped value", got)
	}
}

func TestBindUserIdentityRefusesNoIdentityEvenWithStaleSharedConfig(t *testing.T) {
	ctx := context.Background()

	parent := t.TempDir()
	src := filepath.Join(parent, "src")
	run(t, parent, "git", "init", src)

	gate := filepath.Join(parent, "gate")
	run(t, parent, "git", "init", gate)
	run(t, gate, "git", "config", "user.name", "Stale User")
	run(t, gate, "git", "config", "user.email", "stale@example.com")
	writeFile(t, filepath.Join(gate, "README.md"), "# gate\n")
	run(t, gate, "git", "add", ".")
	run(t, gate, "git", "commit", "-m", "initial")
	dst := filepath.Join(parent, "wt")
	run(t, gate, "git", "worktree", "add", "--detach", dst)

	for _, k := range []string{
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	} {
		if v, ok := os.LookupEnv(k); ok {
			os.Unsetenv(k)
			t.Cleanup(func() { os.Setenv(k, v) })
		}
	}

	err := BindUserIdentity(ctx, src, dst)
	if err == nil {
		t.Fatal("BindUserIdentity succeeded with no bindable identity; stale shared config leaked")
	}
	if !strings.Contains(err.Error(), "cannot bind a git identity") {
		t.Fatalf("error = %q, want bind failure", err)
	}
	// The stale keys must be left untouched - an active run may be using them.
	if got := run(t, dst, "git", "config", "--local", "--get", "--default", "", "user.name"); got != "Stale User" {
		t.Fatalf("stale user.name = %q, want it left untouched", got)
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

func TestUntrackedFilesPreservesRawPaths(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	names := []string{"café.txt", "name with space.txt"}
	if runtime.GOOS != "windows" {
		names = []string{" plain ", "café.txt", "name with space.txt", "tab\tname"}
	}
	for _, name := range names {
		writeFile(t, filepath.Join(dir, name), "new\n")
	}

	files, err := UntrackedFiles(ctx, dir)
	if err != nil {
		t.Fatalf("UntrackedFiles failed: %v", err)
	}
	want := names
	if len(files) != len(want) {
		t.Fatalf("UntrackedFiles = %#v, want %#v", files, want)
	}
	for i, path := range files {
		if path != want[i] {
			t.Fatalf("UntrackedFiles[%d] = %q, want %q", i, path, want[i])
		}
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
