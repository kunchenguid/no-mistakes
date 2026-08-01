//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package branchsync

import "syscall"

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
