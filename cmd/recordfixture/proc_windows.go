//go:build windows

package main

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

func newProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

func terminateCmd(cmd *exec.Cmd, grace time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			return err
		}
		return nil
	case <-time.After(grace):
	}
	if err := cmd.Process.Kill(); err != nil {
		return err
	}
	err := <-done
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return err
	}
	return nil
}
