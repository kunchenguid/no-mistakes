//go:build windows

package e2edaemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsProcessInspection(t *testing.T) {
	command, err := processCommandLine(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(command), "e2edaemon.test") {
		t.Fatalf("unexpected command line %q", command)
	}
	alive, err := processAliveOS(os.Getpid())
	if err != nil || !alive {
		t.Fatalf("processAliveOS = %v, %v", alive, err)
	}
}

func TestWindowsInventoryLockReleasedAfterProcessKill(t *testing.T) {
	if os.Getenv("E2EDAEMON_LOCK_HELPER") == "1" {
		unlock, err := lockFile(os.Getenv("E2EDAEMON_LOCK_PATH"))
		if err != nil {
			os.Exit(2)
		}
		defer unlock()
		if err := os.WriteFile(os.Getenv("E2EDAEMON_LOCK_READY"), []byte("ready"), 0o600); err != nil {
			os.Exit(3)
		}
		time.Sleep(time.Minute)
		return
	}

	dir := t.TempDir()
	lockPath := filepath.Join(dir, "inventory.lock")
	readyPath := filepath.Join(dir, "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsInventoryLockReleasedAfterProcessKill$")
	cmd.Env = append(os.Environ(),
		"E2EDAEMON_LOCK_HELPER=1",
		"E2EDAEMON_LOCK_PATH="+lockPath,
		"E2EDAEMON_LOCK_READY="+readyPath,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("helper did not acquire lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	unlock, err := lockFile(lockPath)
	if err != nil {
		t.Fatalf("lock after killed holder: %v", err)
	}
	unlock()
}
