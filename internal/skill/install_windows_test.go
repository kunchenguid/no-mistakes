//go:build windows

package skill

import (
	"path/filepath"
	"testing"
)

func TestResolveThroughSymlinksPreservesWindowsVolume(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing", "skills", Name)
	got, err := resolveThroughSymlinks(dir)
	if err != nil {
		t.Fatalf("resolveThroughSymlinks: %v", err)
	}
	if got != dir {
		t.Fatalf("resolveThroughSymlinks(%q) = %q, want %q", dir, got, dir)
	}
}
