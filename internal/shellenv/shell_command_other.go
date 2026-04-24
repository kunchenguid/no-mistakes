//go:build !unix && !windows

package shellenv

import "os/exec"

func configureShellCommand(cmd *exec.Cmd) {}
