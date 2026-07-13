//go:build windows

package daemon

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"golang.org/x/sys/windows"
)

const windowsStillActive = 259

// psListDaemonProcessesScript is the PowerShell that emits one
// "<pid>\t<commandline>" line per process with a command line, consumed by
// listDaemonProcesses. The backtick-t is PowerShell's tab inside the
// double-quoted interpolated string.
const psListDaemonProcessesScript = "$ErrorActionPreference='SilentlyContinue'; Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -ne $null } | ForEach-Object { \"$($_.ProcessId)`t$($_.CommandLine)\" }"

// listDaemonProcesses enumerates running processes via PowerShell CIM and
// returns the ones that look like `no-mistakes daemon run --root <root>`. It
// powers the collision detection in reconcileCollidingDaemons on Windows.
// Failures (e.g. PowerShell absent) fail open in the caller.
func listDaemonProcesses() ([]daemonProcessInfo, error) {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", psListDaemonProcessesScript)
	winproc.Harden(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	return parseDaemonProcessOutput(string(out), splitWindowsProcessLine), nil
}

// splitWindowsProcessLine parses one "<pid>\t<commandline>" line emitted by the
// PowerShell CIM script.
func splitWindowsProcessLine(line string) (int, string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	idx := strings.IndexByte(trimmed, '\t')
	if idx <= 0 {
		return 0, "", false
	}
	pid, err := strconv.Atoi(trimmed[:idx])
	if err != nil || pid <= 0 {
		return 0, "", false
	}
	return pid, strings.TrimLeft(trimmed[idx+1:], " \t"), true
}

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
