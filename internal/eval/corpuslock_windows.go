//go:build windows

package eval

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

const corpusLockByteOffset = 0xFFFFFFFF

func tryLockCorpusFile(file *os.File) (bool, error) {
	overlapped := &windows.Overlapped{Offset: corpusLockByteOffset}
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}

func unlockCorpusFile(file *os.File) error {
	overlapped := &windows.Overlapped{Offset: corpusLockByteOffset}
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
}
