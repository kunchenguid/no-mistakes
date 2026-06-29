//go:build windows

package shellenv

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// HideWindow stops cmd from flashing its own console window when started. Use it
// for one-shot console programs (git, helper tools) that are spawned directly
// with os/exec rather than through ConfigureShellCommand, which already sets
// this flag. The caller is expected to redirect the command's stdio, so no
// visible console is ever needed. Safe to call on a nil-SysProcAttr command and
// idempotent.
func HideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
