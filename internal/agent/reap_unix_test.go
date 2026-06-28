//go:build unix

package agent

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

// TestCodexAgent_Run_ReapsLeakedGrandchildOnCleanExit is the regression test for
// the daemon-crash bug behind the agent-spawning test step.
//
// When a repo has no configured test command, the test step asks the agent to
// run the tests itself. That agent (codex here) spawns a test runner whose
// worker pool can outlive it. ConfigureShellCommand isolates the agent in its
// own process group and installs a cmd.Cancel that SIGKILLs the group - but
// cmd.Cancel only fires on context cancellation. On a clean exit (exit 0)
// nothing reaped the group, so the worker grandchildren leaked. Across runs
// those orphans accumulate (each a multi-hundred-MB worker pool) until the host
// is out of memory and the OS OOM-killer SIGKILLs the daemon, which the next
// daemon start reports as "daemon crashed during execution".
//
// The fake codex backgrounds a grandchild whose stdio is detached (so it does
// not hold the agent's stdout pipe open, which would wedge the parser instead
// of exercising the clean-exit leak path), records its pid, prints a valid
// result, and exits 0. After the fix the deferred TerminateShellCommandGroup
// reaps the group on this success path, so the grandchild is gone once Run
// returns. Before the fix it survived.
func TestCodexAgent_Run_ReapsLeakedGrandchildOnCleanExit(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	bin := writeFakeCodex(t, dir, `#!/bin/sh
# Background a long-lived grandchild that outlives this leader, mirroring a test
# runner's worker pool. Detach its stdio so it does not keep the agent's
# stdout/stderr pipe open.
( sleep 120 >/dev/null 2>&1 ) &
echo $! > "`+pidFile+`"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
exit 0
`, "")

	ca := &codexAgent{bin: bin}
	result, err := ca.Run(context.Background(), RunOpts{Prompt: "run the tests", CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("Run returned error (the daemon would fail the step, not crash): %v", err)
	}
	if result.Text != `{"ok":true}` {
		t.Fatalf("unexpected agent text: %q", result.Text)
	}

	grandchild := waitForPidFile(t, pidFile, 5*time.Second)
	// Once Run has returned, the deferred TerminateShellCommandGroup must have
	// SIGKILLed the whole group. Poll briefly to absorb signal-delivery jitter.
	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL) // do not orphan a real process
		t.Fatalf("grandchild pid %d still alive after clean agent exit; the process group leaked "+
			"(this is the leak that OOM-kills the daemon)", grandchild)
	}
}

func TestCodexAgent_Run_ReapsGrandchildHoldingStdoutPipeOnLeaderExit(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	bin := writeFakeCodex(t, dir, `#!/bin/sh
	( sleep 120 ) &
	echo $! > "`+pidFile+`"
	printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'
	exit 0
	`, "")

	ca := &codexAgent{bin: bin}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type runResult struct {
		result *Result
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		result, err := ca.Run(ctx, RunOpts{Prompt: "run the tests", CWD: t.TempDir()})
		done <- runResult{result: result, err: err}
	}()

	var rr runResult
	select {
	case rr = <-done:
	case <-time.After(1500 * time.Millisecond):
		cancel()
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		t.Fatal("agent run did not return after its leader exited while a grandchild held stdout open")
	}

	if rr.err != nil {
		t.Fatalf("Run returned error: %v", rr.err)
	}
	if rr.result.Text != `{"ok":true}` {
		t.Fatalf("unexpected agent text: %q", rr.result.Text)
	}

	grandchild := waitForPidFile(t, pidFile, 5*time.Second)
	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild pid %d still alive after leader exit with inherited stdout", grandchild)
	}
}

func waitForPidFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			if v, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil && v > 0 {
				return v
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for a pid in %s", path)
	return 0
}

// pidGoneWithin reports whether pid stops existing within the window. kill(pid,
// 0) returns ESRCH once the process is gone (the grandchild reparents to init
// after the leader exits, so init reaps it the moment it is SIGKILLed).
func pidGoneWithin(pid int, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) == syscall.ESRCH
}
