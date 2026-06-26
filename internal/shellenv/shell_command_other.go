//go:build !unix && !windows

package shellenv

import "os/exec"

// ConfigureShellCommand is a no-op on platforms that lack process groups
// (and a process-tree kill primitive). Context cancellation falls back to the
// exec.CommandContext default of terminating the direct child only.
func ConfigureShellCommand(cmd *exec.Cmd) {}

// TerminateShellCommandGroup is a no-op on platforms without a process-tree
// kill primitive, matching ConfigureShellCommand's reduced guarantees.
func TerminateShellCommandGroup(cmd *exec.Cmd) {}
