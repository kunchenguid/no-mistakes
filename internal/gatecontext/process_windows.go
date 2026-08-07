//go:build windows

package gatecontext

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

func processParentPID(pid int) (int, error) {
	if pid <= 1 {
		return 0, nil
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return 0, err
	}
	for {
		if int(entry.ProcessID) == pid {
			return int(entry.ParentProcessID), nil
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if err == windows.ERROR_NO_MORE_FILES {
				return 0, nil
			}
			return 0, err
		}
	}
}
