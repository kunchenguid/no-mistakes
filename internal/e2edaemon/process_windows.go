//go:build windows

package e2edaemon

import (
	"fmt"
	"os"
	"time"
)

func processAliveOS(pid int) (bool, error) {
	// On Windows FindProcess always succeeds; probe via process list.
	cmd, err := processCommandLine(pid)
	if err != nil {
		return false, nil
	}
	return cmd != "", nil
}

func signalProcessOS(pid int, sig os.Signal) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	// Windows has no SIGTERM equivalent for arbitrary processes; Kill is the
	// bounded ownership kill path after argv match.
	_ = sig
	if err := proc.Kill(); err != nil {
		return fmt.Errorf("kill %d: %w", pid, err)
	}
	return nil
}

func terminateDaemonPID(pid int) error {
	if err := signalProcessOS(pid, os.Kill); err != nil {
		return err
	}
	if waitProcessExit(pid, 2*time.Second) {
		return nil
	}
	return fmt.Errorf("pid %d still alive after kill", pid)
}
