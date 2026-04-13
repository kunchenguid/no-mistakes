package cli

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/muesli/termenv"
)

func init() {
	if os.Getenv("NM_HOOK_HELPER") == "1" {
		if err := newRootCmd().Execute(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv("NM_DAEMON") != "1" || os.Getenv("NM_TEST_START_DAEMON") != "1" {
		return
	}
	if err := daemon.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// setupTestRepo creates a git repo with an origin remote in a temp dir and
// sets NM_HOME to an isolated temp dir. Returns the repo path and a cleanup
// function that restores the original working directory and NM_HOME.
func setupTestRepo(t *testing.T) string {
	t.Helper()

	// Create temp dirs for the repo and NM_HOME.
	repoDir := t.TempDir()
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	t.Setenv("NM_TEST_START_DAEMON", "1")

	// Create a bare "origin" to use as the upstream.
	originDir := filepath.Join(t.TempDir(), "origin.git")
	run(t, "", "git", "init", "--bare", originDir)

	// Init repo and add origin.
	run(t, repoDir, "git", "init")
	run(t, repoDir, "git", "config", "user.email", "test@test.com")
	run(t, repoDir, "git", "config", "user.name", "Test")
	run(t, repoDir, "git", "remote", "add", "origin", originDir)

	// Create an initial commit so HEAD exists.
	run(t, repoDir, "git", "commit", "--allow-empty", "-m", "initial")

	// Save and change to the repo dir.
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repoDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(origDir) })
	t.Cleanup(func() {
		p := paths.WithRoot(nmHome)
		_, _ = daemon.IsRunning(p)
		_ = daemon.Stop(p)
		// On Windows, the daemon may hold file locks briefly after stopping.
		if runtime.GOOS == "windows" {
			time.Sleep(500 * time.Millisecond)
		}
	})

	return repoDir
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
}

func waitForDaemonRunning(t *testing.T, p *paths.Paths) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if alive, _ := daemon.IsRunning(p); alive {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("daemon did not become responsive")
}

// chdir changes to the given directory and restores the original on cleanup.
func chdir(t *testing.T, dir string) {
	t.Helper()
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(origDir) })
}

func executeCmd(args ...string) (string, error) {
	cmd := newRootCmd()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func TestRootVersion(t *testing.T) {
	out, err := executeCmd("--version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, buildinfo.Version) {
		t.Errorf("version output %q should contain %q", out, buildinfo.Version)
	}
}

func TestRootHelpListsSubcommandsWithoutTriggeringAttach(t *testing.T) {
	out, err := executeCmd("--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"init", "eject", "attach", "rerun", "status", "runs", "doctor", "daemon", "update"} {
		if !strings.Contains(out, sub) {
			t.Errorf("help output should list %q command, got: %s", sub, out)
		}
	}
	if strings.Contains(out, "No active run") {
		t.Errorf("help output should not trigger attach fallback, got: %s", out)
	}
}

func TestSetColorProfileForOutputUsesAsciiForNonTTY(t *testing.T) {
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)

	lipgloss.SetColorProfile(termenv.TrueColor)
	setColorProfileForOutput(new(bytes.Buffer))

	if lipgloss.ColorProfile() != termenv.Ascii {
		t.Fatalf("ColorProfile = %v, want %v", lipgloss.ColorProfile(), termenv.Ascii)
	}
}

func TestUpdateCommandDevBuild(t *testing.T) {
	out, err := executeCmd("update")
	if err != nil {
		t.Fatalf("update failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "self-update unavailable for development builds") {
		t.Fatalf("unexpected update output: %s", out)
	}
}

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
	nmHome, err := os.MkdirTemp("/tmp", "nmh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(nmHome) })
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
	t.Cleanup(func() { run(t, repoDir, "git", "worktree", "remove", "--force", wtDir) })

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

func TestEjectNotInitialized(t *testing.T) {
	setupTestRepo(t)

	_, err := executeCmd("eject")
	if err == nil {
		t.Fatal("eject should fail when not initialized")
	}
	if !strings.Contains(err.Error(), "not initialized") {
		t.Errorf("error should mention 'not initialized', got: %v", err)
	}
}

// startTestDaemon starts an in-process daemon for integration tests.
func startTestDaemon(t *testing.T, p *paths.Paths, d *db.DB) {
	t.Helper()

	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)

	go func() {
		errCh <- daemon.RunWithResources(p, d)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if alive, _ := daemon.IsRunning(p); alive {
			break
		}
		select {
		case err := <-errCh:
			t.Fatalf("daemon exited before becoming responsive: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if alive, _ := daemon.IsRunning(p); !alive {
		t.Fatal("daemon did not become responsive")
	}

	t.Cleanup(func() {
		_ = daemon.Stop(p)
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop within 3s")
		}
	})
}

func TestRootDefaultsToAttachWithAndWithoutHistory(t *testing.T) {
	setupTestRepo(t)
	nmHome, err := os.MkdirTemp("/tmp", "nmh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(nmHome) })
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)

	// Open DB and init gate directly (avoids EnsureDaemon timeout from CLI init).
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := gate.Init(context.Background(), d, p, "."); err != nil {
		t.Fatal(err)
	}

	// Start an in-process daemon.
	startTestDaemon(t, p, d)

	// Run bare `no-mistakes` (no subcommand) - should default to attach behavior.
	out, err := executeCmd()
	if err != nil {
		t.Fatalf("bare command failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "No active run") {
		t.Errorf("expected 'No active run' output, got: %s", out)
	}
	if !strings.Contains(out, "git push no-mistakes") {
		t.Errorf("expected push instructions, got: %s", out)
	}
	if strings.Contains(out, "Recent runs") {
		t.Errorf("did not expect recent runs before history exists, got: %s", out)
	}

	// Look up the repo to insert runs (use FindGitRoot for macOS symlink consistency).
	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.GetRepoByPath(gitRoot)
	if err != nil {
		t.Fatal(err)
	}

	// Insert enough runs to exercise age formatting and the recent-runs cap.
	timestamps := []int64{
		time.Now().Add(-10 * 24 * time.Hour).Unix(),
		time.Now().Add(-4 * 24 * time.Hour).Unix(),
		time.Now().Add(-26 * time.Hour).Unix(),
		time.Now().Add(-2 * time.Hour).Unix(),
		time.Now().Add(-90 * time.Second).Unix(),
		time.Now().Unix(),
	}
	branches := []string{
		"oldest/skipped",
		"feature/cache",
		"feature/login",
		"fix/crash",
		"fix/lint",
		"feature/recent",
	}
	for i, branch := range branches {
		run, err := d.InsertRun(repo.ID, branch, fmt.Sprintf("head%04d", i), "000000")
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			if err := d.UpdateRunStatus(run.ID, "completed"); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := d.UpdateRunError(run.ID, "lint failed"); err != nil {
				t.Fatal(err)
			}
		}
		setRunCreatedAt(t, p.DB(), run.ID, timestamps[i])
	}

	out, err = executeCmd()
	if err != nil {
		t.Fatalf("bare command failed: %v\noutput: %s", err, out)
	}

	// Should show recent runs.
	if !strings.Contains(out, "Recent runs") {
		t.Errorf("expected 'Recent runs' header, got: %s", out)
	}
	if !strings.Contains(out, "feature/login") {
		t.Errorf("expected branch 'feature/login' in output, got: %s", out)
	}
	if !strings.Contains(out, "fix/crash") {
		t.Errorf("expected branch 'fix/crash' in output, got: %s", out)
	}
	for _, want := range []string{"just now", "1 min ago", "2 hours ago", "1 day ago", "4 days ago"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected age %q in output, got: %s", want, out)
		}
	}
	if strings.Contains(out, "oldest/skipped") {
		t.Errorf("oldest run should be omitted once recent-runs limit is hit, got: %s", out)
	}
	if !strings.Contains(out, "(1 more - run 'no-mistakes runs' to see all)") {
		t.Errorf("expected recent-runs overflow hint, got: %s", out)
	}
	// Should still show push instructions.
	if !strings.Contains(out, "git push no-mistakes") {
		t.Errorf("expected push instructions, got: %s", out)
	}
}

func setRunCreatedAt(t *testing.T, dbPath, runID string, ts int64) {
	t.Helper()

	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()

	if _, err := sqlDB.Exec(`UPDATE runs SET created_at = ?, updated_at = ? WHERE id = ?`, ts, ts, runID); err != nil {
		t.Fatal(err)
	}
}

func TestAttachRunIDWithUnknownRunReturnsHelpfulError(t *testing.T) {
	nmHome, err := os.MkdirTemp("", "nmcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(nmHome) })
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	startTestDaemon(t, p, d)

	out, err := executeCmd("attach", "--run", "missing-run")
	if err == nil {
		t.Fatal("attach should fail for an unknown run ID")
	}
	if !strings.Contains(err.Error(), "run not found") {
		t.Fatalf("attach error should mention missing run, got: %v\noutput: %s", err, out)
	}
}

func TestDaemonStatusAndStopWhenNotRunning(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("daemon", "status")
	if err != nil {
		t.Fatalf("daemon status failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "daemon not running") {
		t.Errorf("expected 'daemon not running', got: %s", out)
	}

	out, err = executeCmd("daemon", "stop")
	if err != nil {
		t.Fatalf("daemon stop should succeed when daemon is not running: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "daemon stopped") {
		t.Errorf("expected 'daemon stopped', got: %s", out)
	}
}

func TestDaemonStatusAndStopRunning(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	startTestDaemon(t, p, d)

	out, err := executeCmd("daemon", "status")
	if err != nil {
		t.Fatalf("daemon status failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "daemon running") {
		t.Errorf("expected 'daemon running', got: %s", out)
	}

	out, err = executeCmd("daemon", "stop")
	if err != nil {
		t.Fatalf("daemon stop failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "daemon stopped") {
		t.Errorf("expected 'daemon stopped', got: %s", out)
	}

	// Verify daemon is actually stopped.
	alive, _ := daemon.IsRunning(p)
	if alive {
		t.Error("daemon should not be running after stop")
	}
}

func TestRerunNotInitialized(t *testing.T) {
	setupTestRepo(t)
	nmHome, err := os.MkdirTemp("", "nmcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(nmHome) })
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	startTestDaemon(t, p, d)

	_, err = executeCmd("rerun")
	if err == nil {
		t.Fatal("rerun should fail when repo is not initialized")
	}
	if !strings.Contains(err.Error(), "not initialized") {
		t.Errorf("error should mention 'not initialized', got: %v", err)
	}
}

func TestRerunStartsPipelineForCurrentBranch(t *testing.T) {
	setupTestRepo(t)
	nmHome, err := os.MkdirTemp("", "nmcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(nmHome) })
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	repo, err := gate.Init(context.Background(), d, p, ".")
	if err != nil {
		t.Fatal(err)
	}

	branch, err := git.CurrentBranch(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	headSHA, err := git.HeadSHA(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertRun(repo.ID, branch, headSHA, "0000000000000000000000000000000000000000"); err != nil {
		t.Fatal(err)
	}
	run(t, ".", "git", "push", "no-mistakes", "HEAD:refs/heads/"+branch)

	startTestDaemon(t, p, d)

	out, err := executeCmd("rerun")
	if err != nil {
		t.Fatalf("rerun failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Rerun started") {
		t.Fatalf("expected rerun started output, got: %s", out)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var active ipc.GetActiveRunResult
	if err := client.Call(ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: repo.ID}, &active); err != nil {
		t.Fatal(err)
	}
	if active.Run == nil {
		t.Fatal("expected an active rerun")
	}
	if active.Run.Branch != branch {
		t.Fatalf("active branch = %q, want %q", active.Run.Branch, branch)
	}
	if active.Run.HeadSHA != headSHA {
		t.Fatalf("active head_sha = %q, want %q", active.Run.HeadSHA, headSHA)
	}
}

func TestRerunFromWorktreeUsesCurrentWorktreeBranch(t *testing.T) {
	repoDir := setupTestRepo(t)
	nmHome, err := os.MkdirTemp("", "nmcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(nmHome) })
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	repo, err := gate.Init(context.Background(), d, p, repoDir)
	if err != nil {
		t.Fatal(err)
	}

	const branch = "wt-rerun-branch"
	run(t, repoDir, "git", "checkout", "-b", branch)
	run(t, repoDir, "git", "checkout", "-")

	wtDir := filepath.Join(t.TempDir(), "worktree")
	run(t, repoDir, "git", "worktree", "add", wtDir, branch)
	t.Cleanup(func() { run(t, repoDir, "git", "worktree", "remove", "--force", wtDir) })

	headSHA, err := git.HeadSHA(context.Background(), wtDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertRun(repo.ID, branch, headSHA, "0000000000000000000000000000000000000000"); err != nil {
		t.Fatal(err)
	}
	run(t, repoDir, "git", "push", "no-mistakes", "HEAD:refs/heads/"+branch)

	startTestDaemon(t, p, d)
	chdir(t, wtDir)

	out, err := executeCmd("rerun")
	if err != nil {
		t.Fatalf("rerun from worktree failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "Rerun started") || !strings.Contains(out, branch) {
		t.Fatalf("expected rerun output for %q, got: %s", branch, out)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var active ipc.GetActiveRunResult
	if err := client.Call(ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: repo.ID}, &active); err != nil {
		t.Fatal(err)
	}
	if active.Run == nil {
		t.Fatal("expected an active rerun from worktree")
	}
	if active.Run.Branch != branch {
		t.Fatalf("active branch = %q, want %q", active.Run.Branch, branch)
	}
	if active.Run.HeadSHA != headSHA {
		t.Fatalf("active head_sha = %q, want %q", active.Run.HeadSHA, headSHA)
	}
}

func TestRootErrorFromNonGitDir(t *testing.T) {
	// Running bare `no-mistakes` from a non-git directory should return an
	// error with a useful message, not fail silently.
	// No NM_TEST_START_DAEMON needed: attachRun now checks for a git repo
	// before starting the daemon, so we never spawn a process here.
	nonGitDir := t.TempDir()
	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, nonGitDir)

	_, err := executeCmd()
	if err == nil {
		t.Fatal("expected error when running from non-git directory, got nil")
	}
	if !strings.Contains(err.Error(), "git repository") {
		t.Errorf("error should mention git repository, got: %v", err)
	}
}
