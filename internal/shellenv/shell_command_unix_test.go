//go:build unix

package shellenv

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestTerminateShellCommandGroup_ReapsSurvivingGrandchild covers the
// success-path leak that cmd.Cancel structurally cannot reach: a leader that
// exits cleanly after backgrounding a long-lived child (a vitest worker pool, a
// build watcher) leaves that child reparented to init and still running.
// TerminateShellCommandGroup SIGKILLs the whole process group so nothing
// survives a normally-exited agent.
func TestTerminateShellCommandGroup_ReapsSurvivingGrandchild(t *testing.T) {
	// The grandchild's own stdio is detached from the leader's stdout pipe so
	// the leader's pipe EOFs immediately on exit and cmd.Output returns fast;
	// only the process-group membership (inherited via Setpgid) keeps it tied
	// to the leader. It prints its pid so the test can watch for its death.
	script := `sleep 30 </dev/null >/dev/null 2>&1 & echo $!`
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", script)
	ConfigureShellCommand(cmd)

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run leader: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse grandchild pid %q: %v", out, err)
	}

	// The leader has exited and been reaped, but the grandchild is still alive.
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("expected grandchild %d alive after leader exit, got %v", childPID, err)
	}

	TerminateShellCommandGroup(cmd)

	if !waitForProcessGone(childPID, 2*time.Second) {
		// Best-effort cleanup so we don't leak the sleeper if the assertion fails.
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		t.Fatalf("grandchild %d still alive after TerminateShellCommandGroup", childPID)
	}
}

// TestTerminateShellCommandGroup_NilProcessIsNoop guards the guard: callers
// defer the teardown right after Start, but a Start failure (or a never-started
// cmd) leaves cmd.Process nil, and the no-op must not panic or signal pid 0.
func TestTerminateShellCommandGroup_NilProcessIsNoop(t *testing.T) {
	TerminateShellCommandGroup(nil)
	TerminateShellCommandGroup(&exec.Cmd{})
}

func waitForProcessGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
