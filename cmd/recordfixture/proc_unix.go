//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// newProcAttr puts the recorded server in its own process group so a
// SIGTERM to the recorder (Ctrl-C) can be propagated cleanly to the
// child without going through the shell's process-group routing.
func newProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func terminateCmd(cmd *exec.Cmd, grace time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	select {
	case err := <-done:
		if err != nil && !isSignalExit(err) {
			return err
		}
		return nil
	case <-time.After(grace):
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	err := <-done
	if err != nil && !isSignalExit(err) {
		return err
	}
	return nil
}

func isSignalExit(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}
