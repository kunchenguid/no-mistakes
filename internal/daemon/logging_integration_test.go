package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/intent"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/logstore"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/safepath"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
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

	started := time.Now()
	if err := startDetachedDaemon(p); err != nil {
		t.Logf("startup failed after %v; logs before cleanup:\n%s", time.Since(started), detachedDaemonStartupDiagnostics(p))
		t.Fatalf("start isolated daemon: %s", sanitizeDaemonStartupDiagnostic(p, err.Error()))
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

const daemonStartupDiagnosticTailBytes = 4096

// Read only bounded tails from the two startup sinks and their retained
// backups. Missing/empty files also locate how far startup got before failing.
func detachedDaemonStartupDiagnostics(p *paths.Paths) string {
	var out strings.Builder
	for _, sink := range []struct {
		path    string
		backups int
	}{
		{p.DaemonBootstrapLog(), logstore.BootstrapPolicy().Backups},
		{p.DaemonLog(), logstore.LifecyclePolicy().Backups},
	} {
		for i := 0; i <= sink.backups; i++ {
			path := sink.path
			if i > 0 {
				path += fmt.Sprintf(".%d", i)
			}
			file, err := os.Open(path)
			if err != nil {
				fmt.Fprintf(&out, "%s: missing=%t error=%q\n", filepath.Base(path), os.IsNotExist(err), sanitizeDaemonStartupDiagnostic(p, err.Error()))
				continue
			}
			info, err := file.Stat()
			if err != nil {
				_ = file.Close()
				fmt.Fprintf(&out, "%s: stat error=%q\n", filepath.Base(path), sanitizeDaemonStartupDiagnostic(p, err.Error()))
				continue
			}
			size := min(info.Size(), int64(daemonStartupDiagnosticTailBytes))
			tail := make([]byte, size)
			n, err := file.ReadAt(tail, info.Size()-size)
			_ = file.Close()
			fmt.Fprintf(&out, "%s: exists=true size=%d tail=%q\n", filepath.Base(path), info.Size(), sanitizeDaemonStartupDiagnostic(p, string(tail[:n])))
			if err != nil && err != io.EOF {
				fmt.Fprintf(&out, "read error=%q\n", sanitizeDaemonStartupDiagnostic(p, err.Error()))
			}
		}
	}
	return out.String()
}

func sanitizeDaemonStartupDiagnostic(p *paths.Paths, text string) string {
	text = strings.ReplaceAll(text, p.Root(), "<test-root>")
	return intent.RedactSecrets(safeurl.RedactText(safepath.RedactText(text)))
}

func TestDetachedDaemonStartupFailureDiagnostics(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	const secret = "password=abcdefghijklmnopqrstuv"
	const credentialURL = "https://user:abcdefghijklmnopqrstuv@example.com/repo"
	// The prefix must be excluded, while the tail and rotated records survive.
	bootstrap := "omitted-prefix" + strings.Repeat("x", daemonStartupDiagnosticTailBytes) + "\nbootstrap-tail " + p.Root() + " /home/private-user/daemon " + secret + " " + credentialURL
	for path, content := range map[string]string{
		p.DaemonBootstrapLog():        bootstrap,
		p.DaemonBootstrapLog() + ".1": "rotated-bootstrap-tail",
		p.DaemonLog():                 "",
		p.DaemonLog() + ".1":          "rotated-daemon-tail",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("NM_TEST_START_DAEMON", "1")
	t.Setenv("NM_DAEMON_HELPER_PROCESS", "block")
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "40ms")
	t.Setenv("NM_TEST_DAEMON_START_POLL_INTERVAL", "5ms")
	if err := startDetachedDaemon(p); err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("expected a real child readiness timeout, got %v", err)
	}
	diagnostic := detachedDaemonStartupDiagnostics(p)
	for _, want := range []string{
		fmt.Sprintf("daemon-bootstrap.log: exists=true size=%d", len(bootstrap)),
		"bootstrap-tail", "rotated-bootstrap-tail", "rotated-daemon-tail",
		"daemon.log: exists=true size=0", "daemon.log.3: missing=true", "<test-root>",
	} {
		if !strings.Contains(diagnostic, want) {
			t.Errorf("startup diagnostic missing %q: %s", want, diagnostic)
		}
	}
	for _, excluded := range []string{"omitted-prefix", p.Root(), "/home/private-user", secret, credentialURL} {
		if strings.Contains(diagnostic, excluded) {
			t.Errorf("startup diagnostic retained excluded content %q", excluded)
		}
	}
	if len(diagnostic) > 2*daemonStartupDiagnosticTailBytes {
		t.Errorf("diagnostic size=%d exceeds the fixture's bounded tails", len(diagnostic))
	}
	t.Logf("real child timeout captured bounded, sanitized current and rotated logs before cleanup")
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
