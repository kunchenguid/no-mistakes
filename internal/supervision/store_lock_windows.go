//go:build windows

package supervision

import (
	"os"

	"golang.org/x/sys/windows"
)

const storeLockOffset = 0xFFFFFFFF

func tryLockStoreFile(file *os.File) error {
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&windows.Overlapped{Offset: storeLockOffset},
	)
}

func lockStoreFile(file *os.File) error {
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		1,
		0,
		&windows.Overlapped{Offset: storeLockOffset},
	)
}

func unlockStoreFile(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{Offset: storeLockOffset})
}
