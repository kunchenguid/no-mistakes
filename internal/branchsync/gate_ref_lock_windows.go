//go:build windows

package branchsync

import (
	"fmt"
	"os"
)

func gateRefFileIdentityValue(info os.FileInfo) string {
	return fmt.Sprintf("%T:%#v", info.Sys(), info.Sys())
}

func acquireGateRefOSLock(file *os.File) error {
	return nil
}

func releaseGateRefOSLock(file *os.File) {}

func removeHeldGateRefLock(lock *gateRefLock) error {
	if lock == nil || lock.path == "" {
		return fmt.Errorf("stamped gate lock handle is unavailable")
	}
	if lock.file != nil {
		if lock.osLocked {
			releaseGateRefOSLock(lock.file)
			lock.osLocked = false
		}
		if err := lock.file.Close(); err != nil {
			lock.file = nil
			return err
		}
		lock.file = nil
	}
	return os.Remove(lock.path)
}
