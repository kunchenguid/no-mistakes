//go:build windows

package daemon

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFile takes a non-blocking exclusive lock on f via LockFileEx. Like
// flock on Unix, this lock is released by the OS when the owning process
// exits or crashes, so a held lock always means a still running holder.
func tryLockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, ol,
	)
}

func unlockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}
