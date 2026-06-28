//go:build unix

package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

const nativeAgentEscapedPipeHelperEnv = "NM_AGENT_NATIVE_PIPE_HELPER"

func TestNativeAgentCommand_WaitDelayClosesEscapedPipeHolder(t *testing.T) {
	dir := t.TempDir()
	readyFile := filepath.Join(dir, "ready")
	pidFile := filepath.Join(dir, "escaped.pid")
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestNativeAgentEscapedPipeHelper$")
	cmd.Env = append(os.Environ(),
		nativeAgentEscapedPipeHelperEnv+"=leader",
		"NM_AGENT_NATIVE_PIPE_READY="+readyFile,
		"NM_AGENT_NATIVE_PIPE_PID="+pidFile,
	)
	shellenv.ConfigureShellCommand(cmd)
	cmd.WaitDelay = 100 * time.Millisecond

	started, err := startNativeAgentCommand(cmd)
	if err != nil {
		t.Fatalf("startNativeAgentCommand: %v", err)
	}
	defer started.closePipes()

	type readResult struct {
		output string
		err    error
	}
	readCh := make(chan readResult, 1)
	go func() {
		out, err := io.ReadAll(started.stdout)
		readCh <- readResult{output: string(out), err: err}
	}()

	var rr readResult
	select {
	case rr = <-readCh:
	case <-time.After(2 * time.Second):
		started.closePipes()
		started.terminate()
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		t.Fatal("stdout reader stayed blocked after the leader exited with an escaped pipe holder")
	}

	escapedPID := waitForPidFile(t, pidFile, 5*time.Second)
	t.Cleanup(func() {
		_ = syscall.Kill(escapedPID, syscall.SIGKILL)
	})
	if !strings.Contains(rr.output, "leader done\n") {
		t.Fatalf("stdout output = %q, want leader output", rr.output)
	}
	if rr.err == nil {
		t.Fatalf("stdout read error = nil, want closed-pipe error")
	}
	if err := started.wait(); !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("wait error = %v, want %v", err, exec.ErrWaitDelay)
	}
}

func TestNativeAgentEscapedPipeHelper(t *testing.T) {
	switch os.Getenv(nativeAgentEscapedPipeHelperEnv) {
	case "leader":
		child := exec.Command(os.Args[0], "-test.run=^TestNativeAgentEscapedPipeHelper$")
		child.Env = append(os.Environ(), nativeAgentEscapedPipeHelperEnv+"=escaped",
			"NM_AGENT_NATIVE_PIPE_READY="+os.Getenv("NM_AGENT_NATIVE_PIPE_READY"))
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		_ = os.WriteFile(os.Getenv("NM_AGENT_NATIVE_PIPE_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		if !waitForNativeAgentPipeHelperReady(os.Getenv("NM_AGENT_NATIVE_PIPE_READY"), 5*time.Second) {
			os.Exit(3)
		}
		_, _ = os.Stdout.WriteString("leader done\nescaped pid " + strconv.Itoa(child.Process.Pid) + "\n")
		os.Exit(0)
	case "escaped":
		_, _ = syscall.Setsid()
		_ = os.WriteFile(os.Getenv("NM_AGENT_NATIVE_PIPE_READY"), []byte("ready"), 0o644)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}

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

func waitForNativeAgentPipeHelperReady(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
