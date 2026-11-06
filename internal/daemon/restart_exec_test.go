package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Issue #1691, and the captain's ruling on #1697 that restart when idle must
// work on Windows too: a real daemon process started through RunRestartable
// stops for a scheduled restart and a daemon from its executable file serves
// the same root again. On macOS and Linux that is the same process, replaced
// in place; on Windows the stopping daemon launches its replacement and exits.
func TestRunRestartableServesAgainAfterAScheduledRestart(t *testing.T) {
	root, err := os.MkdirTemp("", "drst")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		shellShim := filepath.Join(t.TempDir(), "test-shell")
		if err := os.WriteFile(shellShim, []byte("#!/bin/sh\nexec env -0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SHELL", shellShim)
	}
	t.Setenv("NM_HOME", root)
	t.Setenv("NM_TEST_START_DAEMON", "1")
	t.Setenv("NM_DAEMON_HELPER_PROCESS", "restartable")
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "20s")
	t.Setenv("NM_TEST_DAEMON_START_POLL_INTERVAL", "10ms")

	if err := startDetachedDaemon(p); err != nil {
		t.Fatalf("start isolated daemon: %v", err)
	}
	firstPID, err := ReadPID(p)
	if err != nil {
		t.Fatalf("read isolated daemon pid: %v", err)
	}
	currentPID := firstPID
	t.Cleanup(func() {
		if pid, err := ReadPID(p); err == nil {
			currentPID = pid
		}
		shutdownIsolatedDaemon(t, p, currentPID)
	})

	if _, err := ScheduleRestart(p); err != nil {
		t.Fatalf("schedule restart: %v", err)
	}

	// The daemon is idle, so it stops at once; wait for a second launch to
	// be recorded and answer health checks.
	deadline := time.Now().Add(20 * time.Second)
	for {
		logText, _ := os.ReadFile(p.DaemonLog())
		launches := strings.Count(string(logText), `msg="daemon process launched" pid=`)
		if launches >= 2 {
			if alive, _ := daemonHealthCheck(p); alive {
				break
			}
		}
		if time.Now().After(deadline) {
			bootstrap, _ := os.ReadFile(p.DaemonBootstrapLog())
			t.Fatalf("no daemon served again after the restart; launches=%d\ndaemon log:\n%s\nbootstrap log:\n%s", launches, logText, bootstrap)
		}
		time.Sleep(20 * time.Millisecond)
	}

	secondPID, err := ReadPID(p)
	if err != nil {
		t.Fatalf("read restarted daemon pid: %v", err)
	}
	currentPID = secondPID
	if runtime.GOOS == "windows" {
		// The stopping daemon exits once its replacement answers.
		deadline := time.Now().Add(10 * time.Second)
		for {
			running, err := daemonProcessRunning(firstPID)
			if err == nil && !running {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the stopping daemon %d did not exit after its replacement %d was ready", firstPID, secondPID)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if secondPID == firstPID {
			t.Fatalf("restarted daemon pid = %d, want a new process on Windows", secondPID)
		}
	} else if secondPID != firstPID {
		t.Fatalf("restarted daemon pid = %d, want %d: the restart replaces the process in place", secondPID, firstPID)
	}
}
