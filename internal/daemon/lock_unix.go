//go:build !windows

package daemon

import (
	"os"
	"syscall"
)

// tryLockFile takes a non-blocking exclusive flock on f. flock is scoped to
// the open file description, not the process, and the kernel releases it
// automatically when every fd referencing that description closes -
// including on process exit or crash - so a held lock always means a still
// running holder.
func tryLockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
