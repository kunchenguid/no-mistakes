//go:build windows

package daemon

import (
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

const windowsStillActive = 259

// detachedDaemonCreationFlags detaches the spawned daemon from the parent's
// console and process group so the parent CLI can exit cleanly even if the
// caller (e.g. PowerShell `Start-Process -Wait`) is waiting on the whole
// console-attached process tree. See issue #164.
const detachedDaemonCreationFlags = windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP

func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: detachedDaemonCreationFlags,
		HideWindow:    true,
	}
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

func processStartTime(pid int) (time.Time, error) {
	if pid <= 0 {
		return time.Time{}, windows.ERROR_INVALID_PARAMETER
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return time.Time{}, err
	}
	defer windows.CloseHandle(handle)

	var created windows.Filetime
	var exited windows.Filetime
	var kernel windows.Filetime
	var user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, created.Nanoseconds()), nil
}
