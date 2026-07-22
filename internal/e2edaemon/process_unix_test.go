//go:build unix

package e2edaemon

import (
	"os/exec"
	"syscall"
)

func detachTestProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
