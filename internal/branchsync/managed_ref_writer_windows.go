//go:build windows

package branchsync

import "golang.org/x/sys/windows"

func replaceManagedGateRef(lockPath, refPath string) error {
	from, err := windows.UTF16PtrFromString(lockPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(refPath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
