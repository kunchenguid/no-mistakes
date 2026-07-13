//go:build windows

// Package winproc hardens child processes so Windows does not allocate a
// visible console window for them. See issue #287: no-mistakes runs from a
// console-less daemon, so every console child (agents, git, shell, provider
// CLIs, helper commands) would otherwise get a fresh visible console window
// for its lifetime. Harden keeps stdout/stderr pipes intact while suppressing
// the window.
package winproc

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// Harden marks cmd so Windows launches it without a visible console window,
// preserving any creation flags already set (e.g. CREATE_NEW_PROCESS_GROUP)
// and leaving stdout/stderr redirection untouched. It is safe to call on a
// command whose SysProcAttr is nil or already populated, and safe to call
// more than once. It does not apply to intentionally detached background
// processes (which specify DETACHED_PROCESS and already have no window).
func Harden(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
	cmd.SysProcAttr.HideWindow = true
}
