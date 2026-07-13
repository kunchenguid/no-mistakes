//go:build windows

package telemetry

import (
	"os"

	"golang.org/x/sys/windows"
)

const readSurfaceLockOffset = 0xFFFFFFFF

func lockReadSurfaceFile(file *os.File) error {
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		1,
		0,
		&windows.Overlapped{Offset: readSurfaceLockOffset},
	)
}

func unlockReadSurfaceFile(file *os.File) error {
	return windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		1,
		0,
		&windows.Overlapped{Offset: readSurfaceLockOffset},
	)
}
