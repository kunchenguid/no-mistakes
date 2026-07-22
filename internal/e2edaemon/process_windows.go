//go:build windows

package e2edaemon

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsStillActive = 259

func processCommandLine(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)

	size := uint32(0)
	err = windows.NtQueryInformationProcess(handle, windows.ProcessCommandLineInformation, nil, 0, &size)
	if err != nil && err != windows.STATUS_INFO_LENGTH_MISMATCH && err != windows.STATUS_BUFFER_TOO_SMALL && err != windows.STATUS_BUFFER_OVERFLOW {
		return "", err
	}
	if size == 0 {
		return "", fmt.Errorf("empty command line buffer for pid %d", pid)
	}
	buffer := make([]byte, size)
	if err := windows.NtQueryInformationProcess(handle, windows.ProcessCommandLineInformation, unsafe.Pointer(&buffer[0]), size, &size); err != nil {
		return "", err
	}
	return (*windows.NTUnicodeString)(unsafe.Pointer(&buffer[0])).String(), nil
}

func processAliveOS(pid int) (bool, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
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

func FindDaemonsForRoot(nmHome string) ([]int, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return nil, nil
		}
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	var pids []int
	for {
		exe := windows.UTF16ToString(entry.ExeFile[:])
		if strings.EqualFold(exe, "no-mistakes.exe") || strings.EqualFold(exe, "no-mistakes") {
			pid := int(entry.ProcessID)
			command, err := processCommandLine(pid)
			if err != nil {
				alive, aliveErr := processAliveOS(pid)
				if aliveErr != nil || alive {
					return nil, fmt.Errorf("inspect candidate pid %d: %w", pid, err)
				}
			} else if commandMatchesDaemonRoot(command, nmHome) {
				pids = append(pids, pid)
			}
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return nil, fmt.Errorf("enumerate processes: %w", err)
		}
	}
	return pids, nil
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
