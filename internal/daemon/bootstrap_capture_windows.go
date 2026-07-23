package daemon

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func redirectProcessOutput(target *os.File) (func() error, error) {
	savedStdoutHandle, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return nil, fmt.Errorf("get stdout handle: %w", err)
	}
	savedStderrHandle, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	if err != nil {
		return nil, fmt.Errorf("get stderr handle: %w", err)
	}
	savedStdout := os.Stdout
	savedStderr := os.Stderr
	targetHandle := windows.Handle(target.Fd())
	if err := windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, targetHandle); err != nil {
		return nil, fmt.Errorf("redirect stdout: %w", err)
	}
	if err := windows.SetStdHandle(windows.STD_ERROR_HANDLE, targetHandle); err != nil {
		_ = windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, savedStdoutHandle)
		return nil, fmt.Errorf("redirect stderr: %w", err)
	}
	os.Stdout = target
	os.Stderr = target
	return func() error {
		os.Stdout = savedStdout
		os.Stderr = savedStderr
		return errors.Join(
			windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, savedStdoutHandle),
			windows.SetStdHandle(windows.STD_ERROR_HANDLE, savedStderrHandle),
		)
	}, nil
}
