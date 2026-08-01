//go:build !windows

package branchsync

import (
	"fmt"
	"os"
	"syscall"
)

func gateRefFileIdentityValue(info os.FileInfo) string {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("dev:%d:ino:%d", stat.Dev, stat.Ino)
	}
	return fmt.Sprintf("%T:%#v", info.Sys(), info.Sys())
}

func acquireGateRefOSLock(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func releaseGateRefOSLock(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
