//go:build windows

package branchsync

import "os"

func replaceManagedGateRef(lockPath, refPath string) error {
	if err := os.Remove(refPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(lockPath, refPath)
}
