//go:build !windows

package shellenv

import "os/exec"

// HideWindow is a no-op off Windows. Only Windows allocates a console window
// for a console child spawned from a windowless parent; on Unix a subprocess
// never pops a window, so there is nothing to suppress.
func HideWindow(cmd *exec.Cmd) {}
