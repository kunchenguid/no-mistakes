//go:build windows

package daemon

import (
	"fmt"
	"os"
)

// terminateOrphanProcessGroup forcibly terminates the orphaned process.
// Windows lacks a Unix-style process group concept, so this maps to
// TerminateProcess on the top-level PID. Child processes that aren't
// parented via a Job Object will not be cleaned up by this call, but the
// managed server itself will be gone.
func terminateOrphanProcessGroup(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := proc.Kill(); err != nil {
		return fmt.Errorf("kill process %d: %w", pid, err)
	}
	return nil
}
