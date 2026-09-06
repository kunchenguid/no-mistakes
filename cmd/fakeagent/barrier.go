package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

// Polling observes an explicit driver-owned release, not elapsed wall time.
// The caller supplies a deadline so a failed driver cannot strand a fixture.
func waitForReleaseFile(ctx context.Context, path string) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("fixture barrier: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("fixture barrier %q: %w", path, ctx.Err())
		case <-ticker.C:
		}
	}
}
