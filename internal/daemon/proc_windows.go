//go:build windows

package daemon

import "os/exec"

func setSysProcAttr(cmd *exec.Cmd) {
	// No Setsid equivalent on Windows; daemon runs in same session.
}
