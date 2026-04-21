//go:build windows

package daemon

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

var taskkillProcessTree = func(pid int) ([]byte, error) {
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	return cmd.CombinedOutput()
}

// terminateOrphanProcessGroup forcibly terminates the orphaned process tree.
func terminateOrphanProcessGroup(pid int) error {
	out, err := taskkillProcessTree(pid)
	if err != nil {
		alive, runningErr := processRunningFunc(pid)
		if runningErr == nil && !alive {
			return nil
		}
		output := strings.TrimSpace(string(out))
		if output != "" {
			return fmt.Errorf("taskkill pid %d: %w: %s", pid, err, output)
		}
		return fmt.Errorf("taskkill pid %d: %w", pid, err)
	}
	return nil
}
