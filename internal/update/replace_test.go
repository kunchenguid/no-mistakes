package update

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReplaceExecutableDarwinRequiresAtomicReplace(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-specific behavior")
	}

	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	execPath := filepath.Join(dir, "no-mistakes")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
	})

	err := replaceExecutable(execPath, []byte("new-binary"))
	if err == nil {
		t.Fatal("replaceExecutable should fail when atomic replacement is unavailable on darwin")
	}
	if !strings.Contains(err.Error(), "reinstall") {
		t.Fatalf("replaceExecutable error = %v", err)
	}
	content, readErr := os.ReadFile(execPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(content) != "old-binary" {
		t.Fatalf("executable content = %q", string(content))
	}
}
