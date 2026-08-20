//go:build unix

package shellenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
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

// TestTerminateShellCommandGroup_AsksBeforeKilling pins that a surviving
// group member is given the chance to shut down: it receives SIGTERM and its
// handler runs to completion. SIGKILL would deny a test runner or worker
// script the chance to flush output and clean up its own temporary state.
//
// A /bin/sh grandchild with `trap` and `sleep` is not this contract: macOS
// /bin/sh (bash 3.2) delivers process-group SIGTERM to both the shell and its
// sleep child, then exits without running the trap. The ready-file handshake
// only proved trap was registered, which is why this test still failed in
// 0.01s on CI run 31827318230 after that fix. The grandchild is a Go helper
// that uses signal.Notify, so SIGTERM is observed by this process, not a
// sleep(1) that dies with the default action.
func TestTerminateShellCommandGroup_AsksBeforeKilling(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	termFile := filepath.Join(dir, "grandchild.term")
	readyFile := filepath.Join(dir, "grandchild.ready")

	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestTerminateShellCommandGroupTermHelper$")
	cmd.Env = append(os.Environ(),
		"NM_SHELLENV_TERM_HELPER=leader",
		"NM_SHELLENV_TERM_PID="+pidFile,
		"NM_SHELLENV_TERM_READY="+readyFile,
		"NM_SHELLENV_TERM_FILE="+termFile,
	)
	ConfigureShellCommand(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("leader Run: %v", err)
	}
	grandchild := readPID(t, pidFile, 5*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })

	TerminateShellCommandGroup(cmd)

	if !pidGoneWithin(grandchild, 5*time.Second) {
		t.Fatalf("grandchild %d still alive after TerminateShellCommandGroup", grandchild)
	}
	if _, err := os.Stat(termFile); err != nil {
		t.Fatalf("grandchild never ran its SIGTERM handler: %v", err)
	}
}

func TestTerminateShellCommandGroupTermHelper(t *testing.T) {
	switch os.Getenv("NM_SHELLENV_TERM_HELPER") {
	case "leader":
		child := exec.Command(os.Args[0], "-test.run=^TestTerminateShellCommandGroupTermHelper$")
		child.Env = append(os.Environ(),
			"NM_SHELLENV_TERM_HELPER=grandchild",
			"NM_SHELLENV_TERM_PID="+os.Getenv("NM_SHELLENV_TERM_PID"),
			"NM_SHELLENV_TERM_READY="+os.Getenv("NM_SHELLENV_TERM_READY"),
			"NM_SHELLENV_TERM_FILE="+os.Getenv("NM_SHELLENV_TERM_FILE"),
		)
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if !waitForHelperReady(os.Getenv("NM_SHELLENV_TERM_READY"), 5*time.Second) {
			_ = child.Process.Kill()
			os.Exit(3)
		}
		os.Exit(0)
	case "grandchild":
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		if err := os.WriteFile(os.Getenv("NM_SHELLENV_TERM_PID"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			os.Exit(4)
		}
		if err := os.WriteFile(os.Getenv("NM_SHELLENV_TERM_READY"), []byte("ready"), 0o644); err != nil {
			os.Exit(5)
		}
		<-term
		if err := os.WriteFile(os.Getenv("NM_SHELLENV_TERM_FILE"), []byte("terminated"), 0o644); err != nil {
			os.Exit(6)
		}
		os.Exit(0)
	default:
		t.Skip("helper invoked by TestTerminateShellCommandGroup_AsksBeforeKilling")
	}
}

// TestTerminateShellCommandGroup_EscalatesWhenSIGTERMIsIgnored is the other
// half of the contract: politeness must not become a new way to leak. A group
// member that ignores SIGTERM is SIGKILLed once the grace period is up.
func TestTerminateShellCommandGroup_EscalatesWhenSIGTERMIsIgnored(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")

	script := "( trap '' TERM; while :; do sleep 0.1; done ) >/dev/null 2>&1 & echo $! > " + pidFile + "; exit 0"
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", script)
	ConfigureShellCommand(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("leader Run: %v", err)
	}
	grandchild := readPID(t, pidFile, 5*time.Second)

	TerminateShellCommandGroup(cmd)

	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild %d ignored SIGTERM and was never escalated to SIGKILL", grandchild)
	}
}

// TestConfigureShellCommand_CancelEscalatesWithoutBlockingWait pins the
// cancellation path: cmd.Cancel runs on the goroutine that owns cmd.Wait, so
// it must return promptly rather than sit through the grace period, and the
// SIGKILL escalation still has to land on a group member that ignores
// SIGTERM.
func TestConfigureShellCommand_CancelEscalatesWithoutBlockingWait(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	script := "( trap '' TERM; while :; do sleep 0.1; done ) >/dev/null 2>&1 & echo $! > " + pidFile + "; " +
		"trap '' TERM; while :; do sleep 0.1; done"
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	ConfigureShellCommand(cmd)
	if err := StartShellCommand(cmd); err != nil {
		t.Fatalf("StartShellCommand: %v", err)
	}
	grandchild := readPID(t, pidFile, 5*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })

	cancel()
	_ = cmd.Wait()

	if !pidGoneWithin(grandchild, 10*time.Second) {
		t.Fatalf("grandchild %d survived cancellation", grandchild)
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
