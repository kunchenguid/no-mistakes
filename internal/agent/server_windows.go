//go:build windows

package agent

import "os/exec"

func configureManagedServerCmd(cmd *exec.Cmd) {}

func signalManagedProcess(cmd *exec.Cmd, force bool) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
