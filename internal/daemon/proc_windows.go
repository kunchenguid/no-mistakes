//go:build windows

package daemon

import (
	"os/exec"

	"golang.org/x/sys/windows"
)

const windowsStillActive = 259

func setSysProcAttr(cmd *exec.Cmd) {
	// No Setsid equivalent on Windows; daemon runs in same session.
}

func processRunning(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if err == windows.ERROR_INVALID_PARAMETER {
			return false, nil
		}
		return false, err
	}
	defer windows.CloseHandle(handle)

	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false, err
	}
	return exitCode == windowsStillActive, nil
}
