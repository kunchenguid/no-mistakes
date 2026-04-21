//go:build windows

package daemon

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// terminateOrphanProcessGroup forcibly terminates the orphaned process tree.
func terminateOrphanProcessGroup(pid int) error {
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := strings.TrimSpace(string(out))
		if output != "" {
			return fmt.Errorf("taskkill pid %d: %w: %s", pid, err, output)
		}
		return fmt.Errorf("taskkill pid %d: %w", pid, err)
	}
	return nil
}
