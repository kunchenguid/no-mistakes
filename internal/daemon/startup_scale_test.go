package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestColdDetachedStartupProductionGateCardinality reproduces the production
// threshold crossing from the v1.41.0 incident with the same 70-gate shape.
// The git shim adds one small cold-filesystem delay per gate so the old
// six-command-per-gate recovery reliably exceeds its five-second startup
// deadline on any CI host. The daemon must nevertheless become ready through a
// real IPC health call under the production readiness budget.
func TestColdDetachedStartupProductionGateCardinality(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX shell shims and Unix process cleanup")
	}

	zeroDuration := startColdDetachedFixture(t, 0, false)
	seventyDuration := startColdDetachedFixture(t, 70, true)
	t.Logf("cold detached startup: zero_gates=%v seventy_gates=%v", zeroDuration, seventyDuration)

	if zeroDuration >= 5*time.Second {
		t.Fatalf("zero-gate cold startup took %v, want below the former 5s deadline", zeroDuration)
	}
	if seventyDuration <= 5*time.Second {
		t.Fatalf("70-gate fixture took %v, want a faithful crossing of the former 5s deadline", seventyDuration)
	}
}

func startColdDetachedFixture(t *testing.T, gateCount int, delayedGit bool) time.Duration {
	t.Helper()

	root, err := os.MkdirTemp("", fmt.Sprintf("nm-startup-%d-", gateCount))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for i := 0; i < gateCount; i++ {
		id := fmt.Sprintf("gate-%02d", i)
		if err := git.InitBare(ctx, p.RepoDir(id)); err != nil {
			_ = database.Close()
			t.Fatalf("init gate %d: %v", i, err)
		}
		if _, err := database.InsertRepoWithID(id, filepath.Join(root, "sources", id), "https://example.com/owner/"+id+".git", "main"); err != nil {
			_ = database.Close()
			t.Fatalf("register gate %d: %v", i, err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	if delayedGit {
		realGit, err := exec.LookPath("git")
		if err != nil {
			t.Fatal(err)
		}
		shimDir := t.TempDir()
		gitShim := filepath.Join(shimDir, "git")
		shim := "#!/bin/sh\n" +
			"if [ \"$2\" = config ] && [ \"$3\" = receive.advertisePushOptions ]; then /bin/sleep 0.075; fi\n" +
			"exec \"$NM_TEST_REAL_GIT\" \"$@\"\n"
		if err := os.WriteFile(gitShim, []byte(shim), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("NM_TEST_REAL_GIT", realGit)
		t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}

	// Run() resolves a login-shell environment before recovery. This isolated
	// shell preserves the fixture PATH without reading the developer's profile.
	shellShim := filepath.Join(t.TempDir(), "test-shell")
	if err := os.WriteFile(shellShim, []byte("#!/bin/sh\nexec env -0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shellShim)
	t.Setenv("NM_TEST_START_DAEMON", "1")
	t.Setenv("NM_DAEMON_HELPER_PROCESS", "daemon")
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "")
	t.Setenv("NM_TEST_DAEMON_START_POLL_INTERVAL", "20ms")

	started := time.Now()
	if err := startDetachedDaemon(p); err != nil {
		t.Fatalf("cold detached startup with %d gates: %v", gateCount, err)
	}
	elapsed := time.Since(started)

	alive, err := daemonIsRunningViaIPC(p)
	if err != nil || !alive {
		t.Fatalf("real IPC health after startup with %d gates = %v, %v", gateCount, alive, err)
	}
	pid, err := ReadPID(p)
	if err != nil {
		t.Fatalf("read isolated daemon pid: %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			shutdownIsolatedDaemon(t, p, pid)
		}
	})
	// Parent readiness observes ServeReady before the child finishes its own
	// confirmLocalIPCHealth and emits phase=ipc_health / "daemon ready". Poll
	// so a fast parent cannot race the log assertion.
	requiredFragments := []string{
		"phase=environment",
		"phase=database",
		"phase=gate_migration",
		fmt.Sprintf("gate_count=%d", gateCount),
		"phase=stale_runs",
		"phase=worktree_cleanup",
		"phase=ipc_bind",
		"phase=ipc_health",
		"msg=\"daemon ready\"",
	}
	logData := waitForDaemonLogFragments(t, p.DaemonLog(), requiredFragments, 2*time.Second)
	if evidenceDir := os.Getenv("NM_TEST_STARTUP_EVIDENCE_DIR"); evidenceDir != "" {
		if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
			t.Fatalf("create startup evidence directory: %v", err)
		}
		evidencePath := filepath.Join(evidenceDir, fmt.Sprintf("startup-%d-gates.log", gateCount))
		if err := os.WriteFile(evidencePath, logData, 0o644); err != nil {
			t.Fatalf("write startup evidence: %v", err)
		}
		t.Logf("startup evidence: %s", evidencePath)
	}
	shutdownIsolatedDaemon(t, p, pid)
	stopped = true
	return elapsed
}

func waitForDaemonLogFragments(t *testing.T, logPath string, fragments []string, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []byte
	var lastErr error
	for {
		last, lastErr = os.ReadFile(logPath)
		if lastErr == nil {
			text := string(last)
			missing := ""
			for _, fragment := range fragments {
				if !strings.Contains(text, fragment) {
					missing = fragment
					break
				}
			}
			if missing == "" {
				return last
			}
			if !time.Now().Before(deadline) {
				t.Fatalf("startup log missing %q after %v:\n%s", missing, timeout, last)
			}
		} else if !time.Now().Before(deadline) {
			t.Fatalf("read daemon log: %v", lastErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
