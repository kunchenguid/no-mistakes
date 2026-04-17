package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func init() {
	if os.Getenv("NM_FAKE_BIN") == "1" {
		name := filepath.Base(os.Args[0])
		if ext := filepath.Ext(name); ext != "" {
			name = strings.TrimSuffix(name, ext)
		}
		switch name {
		case "git":
			if len(os.Args) > 1 && os.Args[1] == "--version" {
				fmt.Fprintln(os.Stdout, "git version 9.9.9")
				os.Exit(0)
			}
			os.Exit(1)
		case "gh", "claude":
			os.Exit(0)
		default:
			os.Exit(1)
		}
	}
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

func writeMockClaude(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		path := filepath.Join(dir, "claude.bat")
		script := "@echo off\r\necho {\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"structured_output\":{\"findings\":[],\"summary\":\"clean\"}}\r\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	path := filepath.Join(dir, "claude")
	script := `#!/bin/sh
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"findings":[],"summary":"clean"}}'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
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

func linkTestBinary(t *testing.T, binDir, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		dst := filepath.Join(binDir, name+".cmd")
		content := "@echo off\r\n" +
			"if /I \"%~n0\"==\"git\" (\r\n" +
			"  if \"%1\"==\"--version\" (\r\n" +
			"    echo git version 9.9.9\r\n" +
			"    exit /b 0\r\n" +
			"  )\r\n" +
			"  exit /b 1\r\n" +
			")\r\n" +
			"if /I \"%~n0\"==\"gh\" exit /b 0\r\n" +
			"if /I \"%~n0\"==\"claude\" exit /b 0\r\n" +
			"exit /b 1\r\n"
		if err := os.WriteFile(dst, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		return dst
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(binDir, name)
	if err := os.Link(exe, dst); err == nil {
		return dst
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}

func cleanupWorktree(t *testing.T, repoDir, wtDir string) {
	t.Helper()

	t.Cleanup(func() {
		_ = os.Chdir(repoDir)
		p := paths.WithRoot(os.Getenv("NM_HOME"))
		_ = daemon.Stop(p)
		if runtime.GOOS == "windows" {
			time.Sleep(500 * time.Millisecond)
		}
		if resolved, err := filepath.EvalSymlinks(wtDir); err == nil {
			wtDir = resolved
		}

		ctx := context.Background()
		var err error
		for attempt := 0; attempt < 5; attempt++ {
			err = git.WorktreeRemove(ctx, repoDir, wtDir)
			if err == nil || isMissingWorktreeError(err) {
				return
			}
			if runtime.GOOS != "windows" {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("remove worktree %q: %v", wtDir, err)
	})
}

func isMissingWorktreeError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "is not a working tree")
}

func TestIsMissingWorktreeError(t *testing.T) {
	t.Parallel()

	if isMissingWorktreeError(nil) {
		t.Fatal("nil should not be treated as a missing worktree error")
	}

	err := errors.New("git worktree remove --force C:\\temp\\worktree: exit status 128: fatal: 'C:\\temp\\worktree' is not a working tree")
	if !isMissingWorktreeError(err) {
		t.Fatal("expected missing worktree error to be ignored during cleanup")
	}

	err = errors.New("git worktree remove failed: permission denied")
	if isMissingWorktreeError(err) {
		t.Fatal("unexpected error should not be treated as a missing worktree error")
	}
}

func makeSocketSafeTempDir(t *testing.T) string {
	t.Helper()

	base := os.TempDir()
	if runtime.GOOS != "windows" {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "nmh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
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
