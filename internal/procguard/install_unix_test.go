//go:build unix

package procguard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstall_CreatesShimSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := Install(root); err != nil {
		t.Fatalf("Install: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	for _, name := range shimNames {
		link := filepath.Join(BinDir(root), name)
		target, err := os.Readlink(link)
		if err != nil {
			t.Fatalf("shim %s: %v", name, err)
		}
		if target != exe {
			t.Fatalf("shim %s -> %s, want %s", name, target, exe)
		}
	}
}

func TestInstall_Idempotent(t *testing.T) {
	root := t.TempDir()
	if err := Install(root); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	if err := Install(root); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	// A stale symlink is repointed on reinstall.
	link := filepath.Join(BinDir(root), "pkill")
	_ = os.Remove(link)
	if err := os.Symlink("/nonexistent/stale", link); err != nil {
		t.Fatalf("seed stale link: %v", err)
	}
	if err := Install(root); err != nil {
		t.Fatalf("reinstall over stale: %v", err)
	}
	target, err := os.Readlink(link)
	if err != nil || target == "/nonexistent/stale" {
		t.Fatalf("stale shim not repaired: target=%q err=%v", target, err)
	}
}
