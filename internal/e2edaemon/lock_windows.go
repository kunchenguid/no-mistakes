//go:build windows

package e2edaemon

import (
	"fmt"
	"os"
	"time"
)

// Windows lacks flock; use exclusive create with retries as a best-effort lock.
func lockFile(path string) (unlock func(), err error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := os.OpenFile(path+".active", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			return func() {
				_ = f.Close()
				_ = os.Remove(path + ".active")
			}, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("e2edaemon: lock timeout: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
