//go:build windows

package shellenv

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// HideWindow prevents cmd from popping a console window when it is spawned from
// a process that has no console of its own - which is exactly the daemon's
// situation, since it runs as a background service/scheduled task with no
// attached console.
//
// Without CREATE_NO_WINDOW, a console child launched from a windowless parent
// forces Windows to allocate a console for it, and the "default terminal
// application" hand-off then surfaces that console as a visible window (a
// Windows Terminal window when Terminal is the default, a conhost window
// otherwise). Every agent (claude/codex/...), every git invocation, and every
// gh/glab/az call would otherwise flash a window on the user's screen for the
// life of the child. CREATE_NO_WINDOW makes the allocated console headless.
//
// It is safe to call before Start and composes with any CreationFlags already
// set (for example the CREATE_NEW_PROCESS_GROUP / CREATE_SUSPENDED that
// ConfigureShellCommand adds). Passing a nil cmd is a no-op.
func HideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
