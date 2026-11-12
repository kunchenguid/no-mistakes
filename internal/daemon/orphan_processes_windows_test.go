//go:build windows

package daemon

import (
	"os"
	"testing"
	"time"
)

// startRunTempOrphan only creates dir on Windows: every subprocess is confined
// to a job object the kernel tears down with its tree, so there is no orphan
// to leave behind and procreap's sweep is a no-op (see internal/procreap).
func startRunTempOrphan(t *testing.T, dir string) func(time.Duration) bool {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	return func(time.Duration) bool { return true }
}
