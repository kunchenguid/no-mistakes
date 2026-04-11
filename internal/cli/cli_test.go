package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func init() {
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

func TestRootHelp(t *testing.T) {
	out, err := executeCmd("--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "init") {
		t.Errorf("help output should list init command, got: %s", out)
	}
	if !strings.Contains(out, "eject") {
		t.Errorf("help output should list eject command, got: %s", out)
	}
	if !strings.Contains(out, "update") {
		t.Errorf("help output should list update command, got: %s", out)
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
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if alive, _ := daemon.IsRunning(p); alive {
			goto daemonStarted
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("daemon did not auto-start after init")

daemonStarted:

	// Eject should succeed.
	out, err = executeCmd("eject")
	if err != nil {
		t.Fatalf("eject failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "ejected") {
		t.Errorf("eject output should say 'ejected', got: %s", out)
	}

	// Remote should be gone.
	cmd = exec.Command("git", "remote", "get-url", "no-mistakes")
	cmd.Dir = repoDir
	if err := cmd.Run(); err == nil {
		t.Error("no-mistakes remote should have been removed after eject")
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
	repoDir := setupTestRepo(t)
	nmHome := filepath.Join(t.TempDir(), strings.Repeat("a", 160))
	t.Setenv("NM_HOME", nmHome)

	_, err := executeCmd("init")
	if err == nil {
		t.Fatal("init should fail when daemon startup fails")
	}
	if !strings.Contains(err.Error(), "start daemon") {
		t.Fatalf("init error = %v, want daemon startup failure", err)
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

	go func() {
		daemon.RunWithResources(p, d)
	}()

	// Wait for daemon to be responsive.
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if alive, _ := daemon.IsRunning(p); alive {
			break
		}
		if i == 49 {
			t.Fatal("daemon did not become responsive")
		}
	}

	t.Cleanup(func() {
		daemon.Stop(p)
	})
}

func TestRootDefaultsToAttach(t *testing.T) {
	setupTestRepo(t)
	nmHome := os.Getenv("NM_HOME")
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

	// Run bare `no-mistakes` (no subcommand) — should default to attach behavior.
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
}

func TestRootNoActiveRunShowsHistory(t *testing.T) {
	setupTestRepo(t)
	nmHome := os.Getenv("NM_HOME")
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := gate.Init(context.Background(), d, p, "."); err != nil {
		t.Fatal(err)
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

	// Insert a completed and a failed run.
	r1, _ := d.InsertRun(repo.ID, "feature/login", "abc12345", "000000")
	d.UpdateRunStatus(r1.ID, "completed")
	r2, _ := d.InsertRun(repo.ID, "fix/crash", "def67890", "000000")
	d.UpdateRunError(r2.ID, "lint failed")

	startTestDaemon(t, p, d)

	out, err := executeCmd()
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
	// Should still show push instructions.
	if !strings.Contains(out, "git push no-mistakes") {
		t.Errorf("expected push instructions, got: %s", out)
	}
}

func TestFormatAge(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name    string
		unixSec int64
		want    string
	}{
		{"just now", now.Unix(), "just now"},
		{"1 min", now.Add(-90 * time.Second).Unix(), "1 min ago"},
		{"5 mins", now.Add(-5 * time.Minute).Unix(), "5 mins ago"},
		{"1 hour", now.Add(-90 * time.Minute).Unix(), "1 hour ago"},
		{"3 hours", now.Add(-3 * time.Hour).Unix(), "3 hours ago"},
		{"1 day", now.Add(-30 * time.Hour).Unix(), "1 day ago"},
		{"5 days", now.Add(-5 * 24 * time.Hour).Unix(), "5 days ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatAge(tt.unixSec)
			if got != tt.want {
				t.Errorf("formatAge() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPrintNoActiveRunNoHistory(t *testing.T) {
	// When repoID is empty (e.g. --run with unknown run), should show simple message.
	var buf bytes.Buffer
	printNoActiveRun(&buf, nil, "")
	out := buf.String()
	if !strings.Contains(out, "No active run") {
		t.Errorf("expected 'No active run', got: %s", out)
	}
	if !strings.Contains(out, "git push no-mistakes") {
		t.Errorf("expected push instructions, got: %s", out)
	}
	// Should NOT show "Recent runs" header.
	if strings.Contains(out, "Recent runs") {
		t.Errorf("should not show 'Recent runs' when no repo context, got: %s", out)
	}
}

func TestDaemonStatusNotRunning(t *testing.T) {
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
}

func TestDaemonStatusRunning(t *testing.T) {
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
	if !strings.Contains(out, "daemon running (pid") {
		t.Errorf("expected 'daemon running (pid N)', got: %s", out)
	}
}

func TestDaemonStopRunning(t *testing.T) {
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

	out, err := executeCmd("daemon", "stop")
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

func TestDaemonStopNotRunning(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("daemon", "stop")
	if err != nil {
		t.Fatalf("daemon stop should succeed when daemon is not running: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "daemon stopped") {
		t.Errorf("expected 'daemon stopped', got: %s", out)
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
	if !strings.Contains(out, "rerun started") {
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

func TestRootHelpStillWorks(t *testing.T) {
	// --help should show subcommands, not trigger attach.
	out, err := executeCmd("--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"init", "eject", "attach", "rerun", "status", "runs", "doctor", "daemon"} {
		if !strings.Contains(out, sub) {
			t.Errorf("help output should list %q command, got: %s", sub, out)
		}
	}
}
