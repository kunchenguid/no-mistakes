//go:build windows

package e2edaemon

import "os/exec"

func detachTestProcess(*exec.Cmd) {}
