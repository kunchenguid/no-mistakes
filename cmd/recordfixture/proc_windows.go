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
	err := terminateWithFallback(func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}, func() error {
		return cmd.Process.Kill()
	}, done, grace, func(err error) bool {
		return errors.Is(err, syscall.EWINDOWS)
	})
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return err
	}
	return nil
}
