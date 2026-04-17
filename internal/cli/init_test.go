package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestInitAndEject(t *testing.T) {
	repoDir := setupTestRepo(t)

	// Init should succeed.
	out, err := executeCmd("init")
	if err != nil {
		t.Fatalf("init failed: %v\noutput: %s", err, out)
	}

	// Resolve symlinks for comparison (macOS /var → /private/var).
	resolved, _ := filepath.EvalSymlinks(repoDir)
	if !strings.Contains(out, resolved) {
		t.Errorf("init output should contain repo path %q, got: %s", resolved, out)
	}
	if !strings.Contains(out, "git push no-mistakes") {
		t.Errorf("init output should contain push instructions, got: %s", out)
	}
	if !strings.Contains(out, "|__| |_/") {
		t.Errorf("init output should contain ASCII art banner, got: %s", out)
	}
	if !strings.Contains(out, "Gate initialized") {
		t.Errorf("init output should contain success message, got: %s", out)
	}

	// Verify the no-mistakes remote was added.
	cmd := exec.Command("git", "remote", "get-url", "no-mistakes")
	cmd.Dir = repoDir
	remoteOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("no-mistakes remote not found: %v", err)
	}
	if !strings.Contains(string(remoteOut), ".git") {
		t.Errorf("remote URL should point to bare repo, got: %s", remoteOut)
	}

	p := paths.WithRoot(os.Getenv("NM_HOME"))
	waitForDaemonRunning(t, p)

	// Eject should succeed.
	out, err = executeCmd("eject")
	if err != nil {
		t.Fatalf("eject failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Gate removed") {
		t.Errorf("eject output should say 'Gate removed', got: %s", out)
	}
	if !strings.Contains(out, resolved) {
		t.Errorf("eject output should contain repo path %q, got: %s", resolved, out)
	}

	// Remote should be gone.
	cmd = exec.Command("git", "remote", "get-url", "no-mistakes")
	cmd.Dir = repoDir
	if err := cmd.Run(); err == nil {
		t.Error("no-mistakes remote should have been removed after eject")
	}
}

func TestInitAndEjectFromWorktreeUseMainRepo(t *testing.T) {
	repoDir := setupTestRepo(t)
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)

	resolvedRepoDir, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		resolvedRepoDir = repoDir
	}

	const branch = "wt-init-branch"
	run(t, repoDir, "git", "checkout", "-b", branch)
	run(t, repoDir, "git", "checkout", "-")

	wtDir := filepath.Join(t.TempDir(), "worktree")
	run(t, repoDir, "git", "worktree", "add", wtDir, branch)
	cleanupWorktree(t, repoDir, wtDir)

	chdir(t, wtDir)

	out, err := executeCmd("init")
	if err != nil {
		t.Fatalf("init from worktree failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, resolvedRepoDir) {
		t.Fatalf("init from worktree should report main repo path %q, got: %s", resolvedRepoDir, out)
	}
	if strings.Contains(out, wtDir) {
		t.Fatalf("init from worktree should not report worktree path %q, got: %s", wtDir, out)
	}

	p := paths.WithRoot(os.Getenv("NM_HOME"))
	waitForDaemonRunning(t, p)

	chdir(t, repoDir)
	out, err = executeCmd("status")
	if err != nil {
		t.Fatalf("status from main repo after worktree init failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, resolvedRepoDir) {
		t.Fatalf("status should resolve initialized main repo path %q, got: %s", resolvedRepoDir, out)
	}

	chdir(t, wtDir)
	out, err = executeCmd("eject")
	if err != nil {
		t.Fatalf("eject from worktree failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, resolvedRepoDir) {
		t.Fatalf("eject from worktree should report main repo path %q, got: %s", resolvedRepoDir, out)
	}

	cmd := exec.Command("git", "remote", "get-url", "no-mistakes")
	cmd.Dir = repoDir
	if err := cmd.Run(); err == nil {
		t.Fatal("no-mistakes remote should have been removed after worktree eject")
	}
}

func TestInitAlreadyInitialized(t *testing.T) {
	setupTestRepo(t)

	_, err := executeCmd("init")
	if err != nil {
		t.Fatalf("first init failed: %v", err)
	}

	_, err = executeCmd("init")
	if err == nil {
		t.Fatal("second init should fail")
	}
	if !strings.Contains(err.Error(), "already initialized") {
		t.Errorf("error should mention 'already initialized', got: %v", err)
	}
}

func TestInitRollsBackWhenDaemonStartFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows IPC does not use Unix socket path limits")
	}

	repoDir := setupTestRepo(t)
	nmHome := filepath.Join(t.TempDir(), strings.Repeat("a", 160))
	t.Setenv("NM_HOME", nmHome)
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "200ms")
	t.Setenv("NM_TEST_DAEMON_START_POLL_INTERVAL", "10ms")

	start := time.Now()
	_, err := executeCmd("init")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("init should fail when daemon startup fails")
	}
	if !strings.Contains(err.Error(), "start daemon") {
		t.Fatalf("init error = %v, want daemon startup failure", err)
	}
	if elapsed >= time.Second {
		t.Fatalf("init rollback should fail fast in tests, took %v", elapsed)
	}

	cmd := exec.Command("git", "remote", "get-url", "no-mistakes")
	cmd.Dir = repoDir
	if err := cmd.Run(); err == nil {
		t.Fatal("no-mistakes remote should be removed after failed init")
	}

	p := paths.WithRoot(nmHome)
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.GetRepoByPath(gitRoot)
	if err != nil {
		t.Fatal(err)
	}
	if repo != nil {
		t.Fatal("repo record should be removed after failed init")
	}

	entries, err := os.ReadDir(p.ReposDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no bare repos after failed init, found %d", len(entries))
	}
}

func TestInitNoGitRepo(t *testing.T) {
	tmpDir := t.TempDir()
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	t.Cleanup(func() { os.Chdir(origDir) })

	_, err := executeCmd("init")
	if err == nil {
		t.Fatal("init should fail outside a git repo")
	}
}
