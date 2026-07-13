//go:build windows

package agent

import (
	"os/exec"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// configureManagedServerCmd suppresses the visible console window Windows would
// otherwise allocate for a managed agent server (opencode, rovodev) spawned
// from the console-less daemon. See issue #287.
func configureManagedServerCmd(cmd *exec.Cmd) { winproc.Harden(cmd) }

func signalManagedProcess(cmd *exec.Cmd, force bool) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
