package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveTestDirRemovesReadOnlyTree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home")
	pkg := filepath.Join(dir, "go", "pkg", "mod", "example.com", "m@v1.0.0")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "m.go"), []byte("package m\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	// Mirror the module cache: every directory read-only, deepest first.
	for p := pkg; p != filepath.Dir(dir); p = filepath.Dir(p) {
		if err := os.Chmod(p, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = removeTestDir(dir) })

	if err := removeTestDir(dir); err != nil {
		t.Fatalf("removeTestDir: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("test dir still exists: %v", err)
	}
}
