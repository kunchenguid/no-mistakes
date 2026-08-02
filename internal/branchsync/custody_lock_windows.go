//go:build windows

package branchsync

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

const custodyLockByteOffset = 0xFFFFFFFF

func tryCustodyLock(file *os.File) error {
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0,
		&windows.Overlapped{Offset: custodyLockByteOffset},
	)
}

func unlockCustodyLock(file *os.File) error {
	return windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0, 1, 0,
		&windows.Overlapped{Offset: custodyLockByteOffset},
	)
}

func isCustodyLockContention(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
