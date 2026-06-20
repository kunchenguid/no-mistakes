//go:build unix

package shellenv

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// ConfigureShellCommand isolates cmd in its own process group (Setpgid) and
// installs a cmd.Cancel that SIGKILLs the whole group when cmd's context is
// cancelled. exec.CommandContext otherwise only kills the direct child PID,
// leaving grandchildren (a test runner's worker processes, an agent-spawned
// git/build/editor) running and holding the worktree locked.
//
// Apply this to every long-lived subprocess no-mistakes spawns on behalf of a
// cancellable step/agent invocation.
func ConfigureShellCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
