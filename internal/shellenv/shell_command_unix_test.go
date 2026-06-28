//go:build unix

package shellenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestTerminateShellCommandGroup_ReapsGrandchildAfterCleanExit pins the
// success-path guarantee that keeps the daemon alive: when a leader configured
// with ConfigureShellCommand exits 0 but leaves a grandchild alive in its
// process group (a test runner's worker pool), TerminateShellCommandGroup
// SIGKILLs the whole group. cmd.Cancel only fires on cancellation, so without
// this the grandchild leaks and orphan pools pile up across runs until the host
// OOMs and the OS kills the daemon.
func TestTerminateShellCommandGroup_ReapsGrandchildAfterCleanExit(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	// The leader backgrounds a long-lived grandchild (stdio detached so it does
	// not hold the inherited pipes open), records its pid, and exits 0.
	script := "( sleep 120 >/dev/null 2>&1 ) & echo $! > " + pidFile + "; exit 0"
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", script)
	ConfigureShellCommand(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("leader Run: %v", err)
	}

	grandchild := readPID(t, pidFile, 5*time.Second)
	if syscall.Kill(grandchild, 0) != nil {
		t.Fatalf("precondition failed: grandchild %d should still be alive before reap", grandchild)
	}

	TerminateShellCommandGroup(cmd)

	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild %d still alive after TerminateShellCommandGroup; group leaked", grandchild)
	}
}

// TestTerminateShellCommandGroup_NoopOnNilOrUnstarted guards the cheap safety
// contract: a nil command, or one that was never started (no Process), must be
// a no-op rather than panic or signal an arbitrary pid.
func TestTerminateShellCommandGroup_NoopOnNilOrUnstarted(t *testing.T) {
	TerminateShellCommandGroup(nil)
	cmd := exec.Command("/bin/sh", "-c", "true") // never Start()ed: cmd.Process is nil
	TerminateShellCommandGroup(cmd)
}

func TestCombinedOutputShellCommand_ReturnsCleanExitWithInheritedPipeGrandchild(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "printf 'leader done\\n'; sleep 30 & exit 0")
	ConfigureShellCommand(cmd)
	cmd.WaitDelay = 100 * time.Millisecond

	out, err := CombinedOutputShellCommand(cmd)
	if err != nil {
		t.Fatalf("CombinedOutputShellCommand() error = %v; output %q", err, out)
	}
	if got, want := string(out), "leader done\n"; got != want {
		t.Fatalf("CombinedOutputShellCommand() output = %q, want %q", got, want)
	}
}

func TestCombinedOutputShellCommand_WaitDelayBoundsEscapedPipeHolder(t *testing.T) {
	readyFile := filepath.Join(t.TempDir(), "ready")
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestShellOutputPipeHelper$")
	cmd.Env = append(os.Environ(),
		"NM_SHELLENV_PIPE_HELPER=leader",
		"NM_SHELLENV_PIPE_READY="+readyFile,
	)
	ConfigureShellCommand(cmd)
	cmd.WaitDelay = 100 * time.Millisecond

	out, err := CombinedOutputShellCommand(cmd)
	escapedPID := parseEscapedPID(t, string(out))
	t.Cleanup(func() {
		_ = syscall.Kill(escapedPID, syscall.SIGKILL)
	})
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("CombinedOutputShellCommand() error = %v, want %v; output %q", err, exec.ErrWaitDelay, out)
	}
	if !strings.Contains(string(out), "leader done\n") {
		t.Fatalf("CombinedOutputShellCommand() output = %q, want leader output", out)
	}
}

func TestShellOutputPipeHelper(t *testing.T) {
	switch os.Getenv("NM_SHELLENV_PIPE_HELPER") {
	case "leader":
		child := exec.Command(os.Args[0], "-test.run=^TestShellOutputPipeHelper$")
		child.Env = append(os.Environ(),
			"NM_SHELLENV_PIPE_HELPER=escaped",
			"NM_SHELLENV_PIPE_READY="+os.Getenv("NM_SHELLENV_PIPE_READY"),
		)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if !waitForHelperReady(os.Getenv("NM_SHELLENV_PIPE_READY"), 5*time.Second) {
			os.Exit(3)
		}
		_, _ = os.Stdout.WriteString("leader done\nescaped pid " + strconv.Itoa(child.Process.Pid) + "\n")
		os.Exit(0)
	case "escaped":
		_, _ = syscall.Setsid()
		_ = os.WriteFile(os.Getenv("NM_SHELLENV_PIPE_READY"), []byte("ready"), 0o644)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}

func readPID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if v, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil && v > 0 {
				return v
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for a pid in %s", path)
	return 0
}

func parseEscapedPID(t *testing.T, output string) int {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "escaped pid ") {
			pid, err := strconv.Atoi(strings.TrimPrefix(line, "escaped pid "))
			if err == nil && pid > 0 {
				return pid
			}
		}
	}
	t.Fatalf("output %q did not contain escaped pid", output)
	return 0
}

func waitForHelperReady(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func pidGoneWithin(pid int, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) == syscall.ESRCH
}
