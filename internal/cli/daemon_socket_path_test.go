//go:build !windows

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func makeLongRoot(t *testing.T) string {
	t.Helper()
	parent, err := os.MkdirTemp("", "nm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(parent) })
	root := filepath.Join(parent, strings.Repeat("r", 120))
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestDaemonGateHelpersReportSocketPathTooLong(t *testing.T) {
	root := makeLongRoot(t)
	gate := filepath.Join(root, "repos", "abc.git")
	wantLength := fmt.Sprintf("is %d bytes", len(filepath.Join(root, "socket")))
	for _, args := range [][]string{
		{"daemon", "admit-push", "--gate", gate},
		{"daemon", "notify-push", "--gate", gate, "--ref", "refs/heads/x", "--old", "0", "--new", "1"},
	} {
		out, err := executeCmd(args...)
		if err == nil {
			t.Fatalf("%v: expected an error", args)
		}
		msg := out + err.Error()
		for _, want := range []string{filepath.Join(root, "socket"), wantLength, "-byte limit", "NM_HOME", "physical"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%v: output %q does not mention %q", args, msg, want)
			}
		}
	}
}

func TestPreReceiveHookThroughShortLinkReportsPhysicalSocketPathTooLong(t *testing.T) {
	root := makeLongRoot(t)
	gate := filepath.Join(root, "repos", "abc.git")
	if out, err := exec.Command("git", "init", "--bare", "-q", gate).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	bin := filepath.Join(t.TempDir(), "no-mistakes")
	build := exec.Command("go", "build", "-o", bin, "github.com/kunchenguid/no-mistakes/cmd/no-mistakes")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, out)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	script, err := git.PreReceiveHookScript(gate)
	if err != nil {
		t.Fatal(err)
	}
	script = strings.Replace(script, "NM_BIN='"+exe+"'", "NM_BIN='"+bin+"'", 1)
	hook := filepath.Join(gate, "hooks", "pre-receive")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(makeSocketSafeTempDir(t), "h")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	linkedGate := filepath.Join(link, "repos", "abc.git")

	cmd := exec.Command("/bin/sh", hook)
	cmd.Dir = linkedGate
	cmd.Env = append(os.Environ(), "PWD="+linkedGate, "NM_HOME="+link, "GIT_DIR="+linkedGate)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("hook should refuse the push; output %q", out)
	}
	t.Logf("Enrolled hook through a short symlink refused admission:\n%s", out)
	wantSocket := filepath.Join(root, "socket")
	for _, want := range []string{wantSocket, fmt.Sprintf("is %d bytes", len(wantSocket)), "-byte limit", "gate push refused"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("hook output %q does not mention %q", out, want)
		}
	}
	if strings.Contains(string(out), filepath.Join(link, "socket")) {
		t.Errorf("hook output names the link path: %q", out)
	}
}

func TestDaemonStartRestartAndStatusReportSocketPathTooLong(t *testing.T) {
	root := makeLongRoot(t)
	t.Setenv("NM_HOME", root)

	originalStart, originalStop, originalRunning := daemonStartFn, daemonStopFn, daemonIsRunningFn
	t.Cleanup(func() { daemonStartFn, daemonStopFn, daemonIsRunningFn = originalStart, originalStop, originalRunning })
	daemonStartFn = func(*paths.Paths) error {
		t.Fatal("daemon start went past the socket path check")
		return nil
	}
	daemonStopFn = func(*paths.Paths) error {
		t.Fatal("daemon stop went past the socket path check")
		return nil
	}
	daemonIsRunningFn = func(*paths.Paths) (bool, error) {
		t.Fatal("daemon status went past the socket path check")
		return false, nil
	}

	for _, sub := range []string{"start", "restart", "status"} {
		out, err := executeCmd("daemon", sub)
		if err == nil {
			t.Fatalf("daemon %s: expected an error", sub)
		}
		msg := out + err.Error()
		for _, want := range []string{filepath.Join(root, "socket"), "NM_HOME", "limit"} {
			if !strings.Contains(msg, want) {
				t.Errorf("daemon %s: output %q does not mention %q", sub, msg, want)
			}
		}
	}
}
