//go:build darwin

package update

import "os/exec"

func removeQuarantine(path string) {
	cmd := exec.Command("xattr", "-d", "com.apple.quarantine", path)
	_ = cmd.Run()
}
