//go:build unix

package steps

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRunShellCommandWithEnv_KillsGrandchildOnCancel is a regression test for
// orphan subprocesses on cancellation. runShellCommandWithEnv must kill the
// whole process group when its context is cancelled, not just the direct
// shell child. Without Setpgid + cmd.Cancel, exec.CommandContext SIGKILLs only
// the shell parent and a backgrounded grandchild (e.g. a test runner's worker
// process) survives, keeps running, and holds the worktree locked so the next
// run on the same branch cannot proceed.
//
// This test fails if shellenv.ConfigureShellCommand is removed from
// runShellCommandWithEnv: the heartbeat keeps advancing and the PID is never
// reaped within the window.
func TestRunShellCommandWithEnv_KillsGrandchildOnCancel(t *testing.T) {
	dir := t.TempDir()
	heartbeat := filepath.Join(dir, "tick")
	pidFile := filepath.Join(dir, "grandchild.pid")
	// Background a long-running grandchild that writes a monotonic heartbeat
	// (so we can prove it actually stopped executing, not merely got reaped as
	// a zombie), then `wait` so the sh parent stays alive until we cancel. This
	// mirrors the real failure mode: `commands.test: "npm test"` shells out and
	// the node workers outlive the cancelled `sh`.
	script := "i=0; while [ $i -lt 10000 ]; do printf '%s\\n' \"$i\" > " + heartbeat +
		"; sleep 0.1; i=$((i+1)); done & echo $! > " + pidFile + "; wait"

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // never leak the 1000s heartbeat loop if we assert early

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = runShellCommandWithEnv(ctx, dir, nil, script)
	}()

	grandchild := waitForIntFile(t, pidFile, 5*time.Second)
	// Synchronize on the grandchild actually running: wait for the heartbeat to
	// advance at least once before cancelling, so we don't race a slow fork+exec.
	waitForHeartbeatChange(t, heartbeat, 3*time.Second)

	before := readTrimFile(t, heartbeat)
	cancel()

	// The grandchild must stop running promptly: the heartbeat holds steady
	// (process is no longer executing) AND the PID has been reaped (no longer
	// alive). The generous window absorbs subreaper/reparenting jitter.
	if !heartbeatHoldsWithin(t, heartbeat, 5*time.Second) {
		t.Fatalf("grandchild pid %d still running after cancel: heartbeat advanced past %q", grandchild, before)
	}
	if err := syscall.Kill(grandchild, 0); err != syscall.ESRCH {
		t.Fatalf("grandchild pid %d not reaped after cancel (kill -0: %v); want ESRCH", grandchild, err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runShellCommandWithEnv did not return within 5s of cancel")
	}
}

// TestRunShellCommandWithEnv_ReapsGrandchildOnCleanExit is the clean-exit
// counterpart to the cancellation test above, and the configured-test-command
// face of the daemon-crash bug. A `commands.test` like "npm test" shells out and
// can leave a worker pool alive after the shell exits 0. cmd.Cancel only reaps
// the group on cancellation, so on a normal exit those workers leaked; across
// runs they accumulate until the host is out of memory and the OS SIGKILLs the
// daemon. runShellCommandWithEnv now defers shellenv.TerminateShellCommandGroup,
// so the group is reaped on the success path too.
//
// This test fails if that defer is removed: the heartbeat keeps advancing and
// the grandchild is never reaped after the command returns.
func TestRunShellCommandWithEnv_ReapsGrandchildOnCleanExit(t *testing.T) {
	dir := t.TempDir()
	heartbeat := filepath.Join(dir, "tick")
	pidFile := filepath.Join(dir, "grandchild.pid")
	// Background a long-running grandchild that writes a monotonic heartbeat,
	// detach its stdio so it does not hold the command's pipes open, record its
	// pid, then exit 0 immediately WITHOUT waiting - the shell parent returns
	// cleanly while the grandchild keeps running, exactly the leak path.
	script := "( i=0; while [ $i -lt 10000 ]; do printf '%s\\n' \"$i\" > " + heartbeat +
		"; sleep 0.1; i=$((i+1)); done ) >/dev/null 2>&1 & echo $! > " + pidFile + "; exit 0"

	ctx := context.Background()
	if _, _, err := runShellCommandWithEnv(ctx, dir, nil, script); err != nil {
		t.Fatalf("runShellCommandWithEnv: %v", err)
	}

	grandchild := waitForIntFile(t, pidFile, 5*time.Second)
	// After the command returns cleanly, the deferred reap must have killed the
	// whole group: the heartbeat stops advancing AND the pid is gone.
	if !heartbeatHoldsWithin(t, heartbeat, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild pid %d still running after clean exit: heartbeat kept advancing", grandchild)
	}
	if err := syscall.Kill(grandchild, 0); err != syscall.ESRCH {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild pid %d not reaped after clean exit (kill -0: %v); want ESRCH", grandchild, err)
	}
}

func waitForIntFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if v, ok := parseInt(readTrimFile(t, path)); ok {
			return v
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for integer in %s", path)
	return 0
}

func waitForHeartbeatChange(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	first := readTrimFile(t, path)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cur := readTrimFile(t, path); cur != "" && cur != first {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("heartbeat at %s never advanced within %s", path, timeout)
}

// heartbeatHoldsWithin reports whether the value at path stops changing,
// indicating the writing process was killed. It returns true as soon as two
// samples separated by a grace period are equal.
func heartbeatHoldsWithin(t *testing.T, path string, window time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(window)
	prev := readTrimFile(t, path)
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		if cur := readTrimFile(t, path); cur == prev {
			return true
		} else {
			prev = cur
		}
	}
	return false
}

func readTrimFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func parseInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return v, true
}
