//go:build !unix && !windows

package shellenv

import (
	"errors"
	"os/exec"
	"time"
)

// ConfigureShellCommand is a no-op on platforms that lack process groups
// (and a process-tree kill primitive). Context cancellation falls back to the
// exec.CommandContext default of terminating the direct child only.
func ConfigureShellCommand(cmd *exec.Cmd) {}

// ConfigureCooperativeShellCommand is likewise a no-op on these platforms.
func ConfigureCooperativeShellCommand(cmd *exec.Cmd) {}

// StartShellCommand starts cmd on platforms without extra process-tree setup.
// It exists so call sites can use the same lifecycle helpers on every platform.
func StartShellCommand(cmd *exec.Cmd) error {
	return cmd.Start()
}

// TerminateShellCommandGroup is a no-op on platforms without a process-tree kill
// primitive, mirroring ConfigureShellCommand. The reap-the-group-on-exit
// guarantee is best-effort and platform-gated.
func TerminateShellCommandGroup(cmd *exec.Cmd) {}

func ShellCommandGroupActive(cmd *exec.Cmd, _ time.Duration) (bool, string, error) {
	return false, "", errors.New("process-group completion cannot be verified on this platform")
}

// detachFromTerminal is a no-op where there is no POSIX session or controlling
// terminal for an interactive shell to contend for.
func detachFromTerminal(cmd *exec.Cmd) {}
