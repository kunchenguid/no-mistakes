package agent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestStartServerWithPort_DetectsEarlyExit verifies that when the spawned
// server exits before becoming healthy (e.g. `acli` not installed, bad
// flags, or port bind failure), startup fails fast instead of waiting the
// full 30s health-check deadline.
func TestStartServerWithPort_DetectsEarlyExit(t *testing.T) {
	bin, err := exec.LookPath("true")
	if err != nil {
		t.Skip("true binary not available")
	}

	start := time.Now()
	srv, err := startServerWithPort(context.Background(), "test", bin, nil, t.TempDir(), "/healthcheck", 1)
	elapsed := time.Since(start)

	if err == nil {
		srv.shutdown()
		t.Fatal("expected error when server exits before becoming healthy")
	}
	if !strings.Contains(err.Error(), "exit") {
		t.Errorf("error should mention early exit, got: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("should fail fast on early exit, waited %v", elapsed)
	}
}

func TestSetManagedServerOutput_RoutesSubprocessOutput(t *testing.T) {
	// Use `sh -c` to emit known bytes to both stdout and stderr, then exit.
	// This exercises the same fd-inheritance path startServerWithPort uses.
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}

	var buf bytes.Buffer
	SetManagedServerOutput(&buf)
	t.Cleanup(func() { SetManagedServerOutput(nil) })

	// Reproduce the fd-wiring startServerWithPort does so we can assert the
	// writer is honored without needing a real health endpoint.
	cmd := exec.Command(sh, "-c", "echo hello-out; echo hello-err 1>&2")
	cmd.Stdin = nil
	out := currentManagedServerOutput()
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		t.Fatalf("run subprocess: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "hello-out") || !strings.Contains(got, "hello-err") {
		t.Fatalf("managed-server writer did not capture subprocess output, got: %q", got)
	}
}

func TestSetManagedServerOutput_NilResetsToDefault(t *testing.T) {
	SetManagedServerOutput(&bytes.Buffer{})
	SetManagedServerOutput(nil)
	if currentManagedServerOutput() != os.Stderr {
		t.Fatal("nil should reset to os.Stderr")
	}
}

// TestStartServerWithPort_RemovesPIDFileOnEarlyExit proves that when a
// server exits before passing its health check, shutdown() still cleans up
// the tracking file so recovery won't later try to reap a non-existent PID.
func TestStartServerWithPort_RemovesPIDFileOnEarlyExit(t *testing.T) {
	bin, err := exec.LookPath("true")
	if err != nil {
		t.Skip("true binary not available")
	}

	pidsDir := t.TempDir()
	SetServerPIDsDir(pidsDir)
	t.Cleanup(func() { SetServerPIDsDir("") })

	srv, err := startServerWithPort(context.Background(), "test", bin, nil, t.TempDir(), "/healthcheck", 1)
	if err == nil {
		srv.shutdown()
		t.Fatal("expected error when server exits before becoming healthy")
	}

	entries, rdErr := os.ReadDir(pidsDir)
	if rdErr != nil {
		t.Fatalf("read pids dir: %v", rdErr)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected no leftover pid files, got %v", names)
	}
}

// TestManagedServerShutdown_RemovesPIDFile covers the graceful-shutdown
// happy path: a running subprocess whose PID file gets cleaned once the
// process exits.
func TestManagedServerShutdown_RemovesPIDFile(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}

	pidsDir := t.TempDir()
	SetServerPIDsDir(pidsDir)
	t.Cleanup(func() { SetServerPIDsDir("") })

	cmd := exec.Command(sh, "-c", "sleep 30")
	configureManagedServerCmd(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sh: %v", err)
	}

	pidFile := writeServerPIDFile(pidsDir, ServerPIDInfo{
		PID:       cmd.Process.Pid,
		Agent:     "test",
		Bin:       sh,
		Port:      0,
		StartedAt: time.Now().UTC(),
	})
	if pidFile == "" {
		t.Fatal("expected pid file path")
	}

	srv := &managedServer{cmd: cmd, pidFile: pidFile, exited: make(chan struct{})}
	go func() {
		srv.waitErr = cmd.Wait()
		close(srv.exited)
	}()

	srv.shutdown()

	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Errorf("pid file should be removed after shutdown, got err=%v", err)
	}
}
