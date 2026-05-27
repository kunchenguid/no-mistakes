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
// full health-check deadline.
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

// TestDefaultHealthTimeout pins the cold-start budget for a freshly spawned
// managed server. Bumped to 60s to absorb opencode boots of 15s+ when the
// host is under load.
func TestDefaultHealthTimeout(t *testing.T) {
	if defaultHealthTimeout != 60*time.Second {
		t.Errorf("defaultHealthTimeout = %v, want 60s", defaultHealthTimeout)
	}
}

// TestWaitForHealth_TimesOut verifies that when the process stays alive but
// never answers its health endpoint, waitForHealth gives up after the
// configured healthTimeout and reports the duration it waited.
func TestWaitForHealth_TimesOut(t *testing.T) {
	bin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep binary not available")
	}

	cmd := exec.Command(bin, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	// Port 1 has nothing listening, so health probes fail with connection
	// refused but the process never exits — exercising the deadline path.
	srv := &managedServer{cmd: cmd, port: 1, exited: make(chan struct{}), healthTimeout: 100 * time.Millisecond}
	go func() {
		srv.waitErr = cmd.Wait()
		close(srv.exited)
	}()

	start := time.Now()
	err = srv.waitForHealth(context.Background(), "/healthcheck")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "health check timed out") {
		t.Errorf("error should mention health-check timeout, got: %v", err)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("returned before deadline, waited only %v", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("waited far past short deadline: %v", elapsed)
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
