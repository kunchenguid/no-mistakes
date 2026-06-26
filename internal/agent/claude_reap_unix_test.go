//go:build unix

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestClaudeAgent_RunOnce_ReapsGrandchildOnNormalExit is the end-to-end guard
// for the success-path leak: a fake claude that backgrounds a long-lived
// grandchild (standing in for a vitest tinypool worker) and then exits 0 must
// not leave that grandchild running once runOnce returns. The fix is the
// deferred TerminateShellCommandGroup; without it the grandchild reparents to
// init and survives, which is the real-world RAM-exhaustion bug.
func TestClaudeAgent_RunOnce_ReapsGrandchildOnNormalExit(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	// The grandchild detaches its stdio from claude's stdout pipe so the pipe
	// EOFs immediately on the leader's exit; only Setpgid group membership ties
	// it to the leader. The leader emits one valid result event and exits 0.
	bin := filepath.Join(dir, "fake-claude")
	script := "#!/bin/sh\n" +
		"sleep 30 </dev/null >/dev/null 2>&1 &\n" +
		"echo $! > \"" + pidFile + "\"\n" +
		`printf '%s\n' '{"type":"result","subtype":"success","structured_output":null}'` + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	a := &claudeAgent{bin: bin}
	if _, err := a.runOnce(context.Background(), RunOpts{CWD: dir}); err != nil {
		t.Fatalf("runOnce: %v", err)
	}

	childPID := readPID(t, pidFile)
	if !waitForPIDGone(childPID, 2*time.Second) {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		t.Fatalf("grandchild %d still alive after runOnce returned; process group was not reaped", childPID)
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	// The grandchild is backgrounded, so the pid file may lag the leader's exit
	// by a scheduler tick.
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild pid file %s never appeared: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForPIDGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
