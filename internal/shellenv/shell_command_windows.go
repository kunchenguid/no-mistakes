//go:build windows

package shellenv

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// CREATE_NEW_PROCESS_GROUP is the Windows creation flag that makes the child
// the root of a new process group, mirroring the unix Setpgid behavior. It is
// the foundation for whole-tree cancellation: taskkill /T walks the tree rooted
// at this process.
const createNewProcessGroup = 0x00000200

// ConfigureShellCommand is the Windows counterpart to the unix helper. There
// are no Unix-style process groups or signals here, so cancellation runs
// `taskkill /T /F /PID`, which terminates the process and everything it
// spawned (test runners, agent-spawned git/build tools). Without this,
// exec.CommandContext only TerminateProcess-es the direct child and leaks the
// grandchildren, keeping the worktree locked.
//
// cmd.Cancel returns os.ErrProcessDone when the process is already gone so the
// exec package stops waiting, matching the unix variant's ESRCH handling.
func ConfigureShellCommand(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		// taskkill returns a non-zero exit code (and error) when the PID no
		// longer exists; that is the "already done" case we must signal.
		if err := kill.Run(); err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				return err
			}
			return os.ErrProcessDone
		}
		return nil
	}
}
