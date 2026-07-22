//go:build unix

package e2edaemon

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

func processAliveOS(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	return false, err
}

func signalProcessOS(pid int, sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		s = syscall.SIGTERM
	}
	return syscall.Kill(pid, s)
}

func terminateDaemonPID(pid int) error {
	_ = signalProcessOS(pid, syscall.SIGTERM)
	if waitProcessExit(pid, 2*time.Second) {
		return nil
	}
	_ = signalProcessOS(pid, syscall.SIGKILL)
	if waitProcessExit(pid, 2*time.Second) {
		return nil
	}
	return fmt.Errorf("pid %d still alive after SIGKILL", pid)
}
