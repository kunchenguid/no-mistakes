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
