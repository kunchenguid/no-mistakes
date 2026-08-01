//go:build !windows

package branchsync

import "os"

func replaceManagedGateRef(lockPath, refPath string) error {
	return os.Rename(lockPath, refPath)
}
