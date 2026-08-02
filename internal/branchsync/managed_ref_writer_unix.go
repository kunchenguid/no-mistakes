//go:build !windows

package branchsync

import (
	"os"
	"path/filepath"
)

func replaceManagedGateRef(lockPath, refPath string) error {
	if err := os.Rename(lockPath, refPath); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(refPath))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
