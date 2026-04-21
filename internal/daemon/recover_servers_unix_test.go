//go:build !windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// spawnSleepProcess launches `sleep 30` in its own process group so that a
// reap test that kills the pgroup can't take out the go test runner. The
// returned *exec.Cmd lets the caller clean up with killAndWait.
func spawnSleepProcess(t *testing.T) (*exec.Cmd, int) {
	t.Helper()
	bin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	cmd := exec.Command(bin, "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	return cmd, cmd.Process.Pid
}

func killAndWait(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

func waitForPIDExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		alive, _ := processRunning(pid)
		if !alive {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// TestReapOrphanedServers_KillsLiveMatchingProcess proves the full reap
// flow: a PID file whose recorded StartedAt matches the subprocess's real
// start time triggers a kill, and the PID file is removed.
func TestReapOrphanedServers_KillsLiveMatchingProcess(t *testing.T) {
	cmd, pid := spawnSleepProcess(t)
	t.Cleanup(func() { killAndWait(cmd) })

	started, err := processStartTime(pid)
	if err != nil {
		t.Fatalf("read start time: %v", err)
	}

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	path := writePIDRecord(t, p.ServerPIDsDir(), "opencode-live.json", agent.ServerPIDInfo{
		PID:       pid,
		Agent:     "opencode",
		Bin:       "/bin/sleep",
		StartedAt: started,
	})

	reapOrphanedServers(p)

	if !waitForPIDExit(pid, 5*time.Second) {
		t.Errorf("expected pid %d to be terminated", pid)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("pid file should be removed after reap, got err=%v", err)
	}
}

// TestReapOrphanedServers_SkipsWhenDaemonAlive verifies that if the daemon
// PID file points at a running process, reaping is skipped entirely -
// protects a concurrently-running old daemon's legitimate servers.
func TestReapOrphanedServers_SkipsWhenDaemonAlive(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	other, otherPID := spawnSleepProcess(t)
	t.Cleanup(func() { killAndWait(other) })

	if err := os.WriteFile(p.PIDFile(), []byte(fmt.Sprintf("%d", otherPID)), 0o644); err != nil {
		t.Fatal(err)
	}

	path := writePIDRecord(t, p.ServerPIDsDir(), "opencode-999999.json", agent.ServerPIDInfo{
		PID:       999999,
		Agent:     "opencode",
		StartedAt: time.Now().UTC(),
	})

	reapOrphanedServers(p)

	if _, err := os.Stat(path); err != nil {
		t.Errorf("pid file should be untouched when a daemon is alive, got err=%v", err)
	}
}

// TestReapOrphanedServers_SkipsKillWhenStartTimeMismatched simulates PID
// reuse: the PID file's StartedAt doesn't match the live process's actual
// start time, so the reaper must NOT send signals - it just drops the
// stale file.
func TestReapOrphanedServers_SkipsKillWhenStartTimeMismatched(t *testing.T) {
	cmd, pid := spawnSleepProcess(t)
	t.Cleanup(func() { killAndWait(cmd) })

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// Bogus StartedAt far in the past makes this look like a reused PID.
	path := writePIDRecord(t, p.ServerPIDsDir(), "opencode-reused.json", agent.ServerPIDInfo{
		PID:       pid,
		Agent:     "opencode",
		Bin:       "/bin/sleep",
		StartedAt: time.Now().UTC().Add(-24 * time.Hour),
	})

	reapOrphanedServers(p)

	// The process must still be alive - we didn't own it.
	alive, err := processRunning(pid)
	if err != nil {
		t.Fatalf("processRunning: %v", err)
	}
	if !alive {
		t.Error("reaper killed a process whose start time did not match the pid record")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale pid file should still be removed, got err=%v", err)
	}
}

func TestReapOrphanedServers_KeepsPIDFileWhenTerminateFails(t *testing.T) {
	cmd, pid := spawnSleepProcess(t)
	t.Cleanup(func() { killAndWait(cmd) })

	started, err := processStartTime(pid)
	if err != nil {
		t.Fatalf("read start time: %v", err)
	}

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	path := writePIDRecord(t, p.ServerPIDsDir(), "opencode-live.json", agent.ServerPIDInfo{
		PID:       pid,
		Agent:     "opencode",
		Bin:       "/bin/sleep",
		StartedAt: started,
	})

	old := terminateOrphanProcessGroupFunc
	terminateOrphanProcessGroupFunc = func(pid int) error {
		return errors.New("boom")
	}
	t.Cleanup(func() { terminateOrphanProcessGroupFunc = old })

	reapOrphanedServers(p)

	if _, err := os.Stat(path); err != nil {
		t.Errorf("pid file should be kept for retry after terminate failure, got err=%v", err)
	}
	if alive, err := processRunning(pid); err != nil {
		t.Fatalf("processRunning: %v", err)
	} else if !alive {
		t.Error("process should remain alive when terminate hook fails")
	}
}
