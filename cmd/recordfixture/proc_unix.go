//go:build !windows

package main

import "syscall"

// newProcAttr puts the recorded server in its own process group so a
// SIGTERM to the recorder (Ctrl-C) can be propagated cleanly to the
// child without going through the shell's process-group routing.
func newProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
