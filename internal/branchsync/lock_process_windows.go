//go:build windows

package branchsync

import "os"

func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	return err == nil && process != nil
}
