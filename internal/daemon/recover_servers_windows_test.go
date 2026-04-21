//go:build windows

package daemon

import (
	"errors"
	"testing"
)

func TestTerminateOrphanProcessGroup_IgnoresAlreadyExitedTaskkillRace(t *testing.T) {
	oldTaskkill := taskkillProcessTree
	oldRunning := processRunningFunc
	taskkillProcessTree = func(pid int) ([]byte, error) {
		if pid != 12345 {
			t.Fatalf("unexpected pid %d", pid)
		}
		return []byte("ERROR: The process with PID 12345 could not be terminated."), errors.New("exit status 128")
	}
	processRunningFunc = func(pid int) (bool, error) {
		if pid != 12345 {
			t.Fatalf("unexpected pid %d", pid)
		}
		return false, nil
	}
	t.Cleanup(func() {
		taskkillProcessTree = oldTaskkill
		processRunningFunc = oldRunning
	})

	if err := terminateOrphanProcessGroup(12345); err != nil {
		t.Fatalf("terminateOrphanProcessGroup returned error: %v", err)
	}
}
