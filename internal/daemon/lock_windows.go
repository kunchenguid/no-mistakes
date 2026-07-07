//go:build windows

package daemon

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockByteOffset is where the single locked byte lives. Unlike flock,
// LockFileEx byte-range locks are mandatory: a lock overlapping the holder
// record at the start of the file would make readLockHolder's cross-handle
// ReadAt fail with ERROR_LOCK_VIOLATION, so the locked byte sits far past
// any data ever written to the file (range locks may extend beyond EOF).
const lockByteOffset = 0xFFFFFFFF

// tryLockFile takes a non-blocking exclusive lock on f via LockFileEx. Like
// flock on Unix, this lock is released by the OS when the owning process
// exits or crashes, so a held lock always means a still running holder.
func tryLockFile(f *os.File) error {
	ol := &windows.Overlapped{Offset: lockByteOffset}
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, ol,
	)
}

func unlockFile(f *os.File) error {
	ol := &windows.Overlapped{Offset: lockByteOffset}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}
