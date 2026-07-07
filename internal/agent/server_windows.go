//go:build windows

package agent

import (
	"os/exec"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

func configureManagedServerCmd(cmd *exec.Cmd) {
	// Keep managed agent servers (opencode, rovodev) from popping a console
	// window; their stdout/stderr are already routed to the configured sink.
	shellenv.HideWindow(cmd)
}

func signalManagedProcess(cmd *exec.Cmd, force bool) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
