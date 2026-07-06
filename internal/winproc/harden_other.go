//go:build !windows

// Package winproc hardens child processes so Windows does not allocate a
// visible console window for them. On non-Windows platforms there is no such
// concept, so Harden is a no-op.
package winproc

import "os/exec"

// Harden is a no-op on non-Windows platforms.
func Harden(cmd *exec.Cmd) {}
