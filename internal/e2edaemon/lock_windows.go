//go:build windows

package e2edaemon

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

const inventoryLockOffset = 0xFFFFFFFF

func lockFile(path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("e2edaemon: open lock: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ol := &windows.Overlapped{Offset: inventoryLockOffset}
		err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
		if err == nil {
			return func() {
				_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{Offset: inventoryLockOffset})
				_ = f.Close()
			}, nil
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("e2edaemon: lock timeout: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
