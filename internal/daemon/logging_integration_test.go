package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestDetachedDaemonUsesBoundedDedicatedLogSinks is a production-shaped
// regression: a real isolated daemon child starts with service-manager file
// descriptors already open, rotates prior crash output in place, keeps healthy
// read RPCs quiet, and retains an actionable failed-request record.
func TestDetachedDaemonUsesBoundedDedicatedLogSinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("production descriptor inheritance path is covered by platform-neutral logstore tests")
	}
	root, err := os.MkdirTemp("", "dlog")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.DaemonBootstrapLog(), []byte("previous crash diagnostic\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	shellShim := filepath.Join(t.TempDir(), "test-shell")
	if err := os.WriteFile(shellShim, []byte("#!/bin/sh\nexec env -0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shellShim)
	t.Setenv("NM_TEST_START_DAEMON", "1")
	t.Setenv("NM_DAEMON_HELPER_PROCESS", "daemon")
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "10s")
	t.Setenv("NM_TEST_DAEMON_START_POLL_INTERVAL", "10ms")

	if err := startDetachedDaemon(p); err != nil {
		t.Fatalf("start isolated daemon: %v", err)
	}
	pid, err := ReadPID(p)
	if err != nil {
		t.Fatalf("read isolated daemon pid: %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}
		shutdownIsolatedDaemon(t, p, pid)
	})

	backup, err := os.ReadFile(p.DaemonBootstrapLog() + ".1")
	if err != nil {
		t.Fatalf("read rotated bootstrap backup: %v", err)
	}
	if !strings.Contains(string(backup), "previous crash diagnostic") {
		t.Fatalf("bootstrap backup lost crash diagnostic: %q", backup)
	}
	if _, err := os.Stat(p.ManagedServerLog()); err != nil {
		t.Fatalf("dedicated managed-server sink was not created: %v", err)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial isolated daemon: %v", err)
	}
	for i := 0; i < 20; i++ {
		var health ipc.HealthResult
		if err := client.Call(ipc.MethodHealth, &ipc.HealthParams{}, &health); err != nil {
			t.Fatalf("health request %d: %v", i, err)
		}
		var runs ipc.GetRunsResult
		if err := client.Call(ipc.MethodGetRuns, &ipc.GetRunsParams{RepoID: "missing"}, &runs); err != nil {
			t.Fatalf("get_runs request %d: %v", i, err)
		}
	}
	var raw json.RawMessage
	if err := client.Call("broken_method", nil, &raw); err == nil {
		t.Fatal("unknown request unexpectedly succeeded")
	}
	_ = client.Close()

	lifecycle, err := os.ReadFile(p.DaemonLog())
	if err != nil {
		t.Fatalf("read lifecycle log: %v", err)
	}
	text := string(lifecycle)
	for _, quiet := range []string{"method=health", "method=get_runs"} {
		if strings.Contains(text, quiet) {
			t.Errorf("healthy read amplified lifecycle log with %q:\n%s", quiet, text)
		}
	}
	for _, visible := range []string{"msg=\"daemon ready\"", "msg=\"ipc request failed\" method=broken_method"} {
		if !strings.Contains(text, visible) {
			t.Errorf("lifecycle log missing %q:\n%s", visible, text)
		}
	}

	shutdownIsolatedDaemon(t, p, pid)
	stopped = true
}

func shutdownIsolatedDaemon(t *testing.T, p *paths.Paths, pid int) {
	t.Helper()
	if client, err := ipc.Dial(p.Socket()); err == nil {
		_ = client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil)
		_ = client.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		running, err := daemonProcessRunning(pid)
		if err == nil && !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if running, _ := daemonProcessRunning(pid); running {
		_ = daemonKillPID(pid)
		t.Fatalf("isolated daemon pid %d did not exit", pid)
	}
}
