//go:build windows

package agent

import (
	"os/exec"
	"syscall"
)

const managedServerCreateNoWindow = 0x08000000

func configureManagedServerCmd(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= managedServerCreateNoWindow
	cmd.SysProcAttr.HideWindow = true
}

func signalManagedProcess(cmd *exec.Cmd, force bool) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
