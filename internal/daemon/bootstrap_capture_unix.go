//go:build darwin || linux

package daemon

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func redirectProcessOutput(target *os.File) (func() error, error) {
	savedStdout, err := unix.Dup(1)
	if err != nil {
		return nil, fmt.Errorf("duplicate stdout: %w", err)
	}
	savedStderr, err := unix.Dup(2)
	if err != nil {
		_ = unix.Close(savedStdout)
		return nil, fmt.Errorf("duplicate stderr: %w", err)
	}
	if err := unix.Dup2(int(target.Fd()), 1); err != nil {
		_ = unix.Close(savedStdout)
		_ = unix.Close(savedStderr)
		return nil, fmt.Errorf("redirect stdout: %w", err)
	}
	if err := unix.Dup2(int(target.Fd()), 2); err != nil {
		_ = unix.Dup2(savedStdout, 1)
		_ = unix.Close(savedStdout)
		_ = unix.Close(savedStderr)
		return nil, fmt.Errorf("redirect stderr: %w", err)
	}
	return func() error {
		stdoutErr := unix.Dup2(savedStdout, 1)
		stderrErr := unix.Dup2(savedStderr, 2)
		closeStdoutErr := unix.Close(savedStdout)
		closeStderrErr := unix.Close(savedStderr)
		return errors.Join(stdoutErr, stderrErr, closeStdoutErr, closeStderrErr)
	}, nil
}
