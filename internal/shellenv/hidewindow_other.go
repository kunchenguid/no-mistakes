//go:build !windows

package shellenv

import "os/exec"

// HideWindow is a no-op off Windows, where console subprocesses do not pop their
// own windows.
func HideWindow(cmd *exec.Cmd) {}
