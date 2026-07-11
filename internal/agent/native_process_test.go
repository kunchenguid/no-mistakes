package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

const nativeProcessLifecycleHelperEnv = "NO_MISTAKES_NATIVE_PROCESS_LIFECYCLE_HELPER"

func TestStartNativeProcessDelegatesStartFailureToShellLifecycle(t *testing.T) {
	startErr := errors.New("injected shell lifecycle start failure")
	var startCalls atomic.Int32
	var terminateCalls atomic.Int32
	replaceNativeProcessLifecycleForTest(t, nativeProcessLifecycle{
		start: func(*exec.Cmd) error {
			startCalls.Add(1)
			return startErr
		},
		terminate: func(*exec.Cmd) {
			terminateCalls.Add(1)
		},
	})

	missingCommand := filepath.Join(t.TempDir(), "missing-native-process-command")
	started, err := startNativeProcess(exec.Command(missingCommand))
	if started != nil {
		started.closePipes()
		t.Fatal("startNativeProcess() returned a process after lifecycle start failure")
	}
	if !errors.Is(err, startErr) {
		t.Fatalf("startNativeProcess() error = %v, want injected lifecycle error", err)
	}
	if got := startCalls.Load(); got != 1 {
		t.Fatalf("shell lifecycle start calls = %d, want 1", got)
	}
	if got := terminateCalls.Load(); got != 0 {
		t.Fatalf("shell lifecycle terminate calls = %d after start failure, want 0", got)
	}
}

func TestNativeProcessUsesShellLifecycleExactlyOnce(t *testing.T) {
	var startCalls atomic.Int32
	var terminateCalls atomic.Int32
	replaceNativeProcessLifecycleForTest(t, nativeProcessLifecycle{
		start: func(cmd *exec.Cmd) error {
			startCalls.Add(1)
			return shellenv.StartShellCommand(cmd)
		},
		terminate: func(cmd *exec.Cmd) {
			terminateCalls.Add(1)
			shellenv.TerminateShellCommandGroup(cmd)
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeProcessLifecycleHelperProcess$")
	cmd.Env = append(os.Environ(), nativeProcessLifecycleHelperEnv+"=1")
	started, err := startNativeProcess(cmd)
	if err != nil {
		t.Fatalf("startNativeProcess() error = %v", err)
	}
	defer started.closePipes()
	if got := startCalls.Load(); got != 1 {
		started.terminate()
		started.closePipes()
		_ = started.wait()
		t.Fatalf("shell lifecycle start calls = %d, want 1", got)
	}
	if started.pid() <= 0 {
		t.Fatalf("pid() = %d, want a started process PID", started.pid())
	}
	if cmd.WaitDelay != nativeProcessWaitDelay {
		t.Fatalf("WaitDelay = %v, want %v", cmd.WaitDelay, nativeProcessWaitDelay)
	}

	stdout, stdoutErr := io.ReadAll(started.stdout)
	stderr, stderrErr := io.ReadAll(started.stderr)
	if stdoutErr != nil || stderrErr != nil {
		t.Fatalf("read process pipes: stdout error = %v, stderr error = %v", stdoutErr, stderrErr)
	}
	if string(stdout) != "native stdout" {
		t.Fatalf("stdout = %q, want %q", stdout, "native stdout")
	}
	if string(stderr) != "native stderr" {
		t.Fatalf("stderr = %q, want %q", stderr, "native stderr")
	}
	if err := started.wait(); err != nil {
		t.Fatalf("wait() error = %v", err)
	}
	if got := terminateCalls.Load(); got != 1 {
		t.Fatalf("shell lifecycle terminate calls after wait = %d, want 1", got)
	}

	started.terminate()
	started.terminate()
	if got := terminateCalls.Load(); got != 1 {
		t.Fatalf("shell lifecycle terminate calls after repeated termination = %d, want 1", got)
	}
}

func TestNativeProcessLifecycleHelperProcess(t *testing.T) {
	if os.Getenv(nativeProcessLifecycleHelperEnv) != "1" {
		return
	}
	_, _ = fmt.Fprint(os.Stdout, "native stdout")
	_, _ = fmt.Fprint(os.Stderr, "native stderr")
	os.Exit(0)
}

func replaceNativeProcessLifecycleForTest(t *testing.T, lifecycle nativeProcessLifecycle) {
	t.Helper()
	original := nativeProcessShellLifecycle
	nativeProcessShellLifecycle = lifecycle
	t.Cleanup(func() {
		nativeProcessShellLifecycle = original
	})
}
