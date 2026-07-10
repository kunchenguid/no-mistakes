//go:build !windows

package telemetry

import (
	"os"
	"syscall"
)

func lockReadSurfaceFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
}

func unlockReadSurfaceFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
