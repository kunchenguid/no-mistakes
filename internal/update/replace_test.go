package update

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReplaceExecutableWindowsMovesRunningImageAside(t *testing.T) {
	setReplaceTestGOOS(t, "windows")

	execPath := filepath.Join(t.TempDir(), "no-mistakes.exe")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0o751); err != nil {
		t.Fatal(err)
	}

	if err := replaceExecutable(execPath, []byte("new-binary")); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, execPath, "new-binary")
	assertFileContents(t, execPath+".old", "old-binary")
	info, err := os.Stat(execPath)
	if err != nil {
		t.Fatal(err)
	}
	wantPerm := os.FileMode(0o751)
	if runtime.GOOS == "windows" {
		wantPerm = 0o666
	}
	if got := info.Mode().Perm(); got != wantPerm {
		t.Fatalf("replacement permissions = %o, want %o", got, wantPerm)
	}
}

func TestReplaceExecutableWindowsRemovesStaleBackup(t *testing.T) {
	setReplaceTestGOOS(t, "windows")

	execPath := filepath.Join(t.TempDir(), "no-mistakes.exe")
	if err := os.WriteFile(execPath, []byte("current-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(execPath+".old", []byte("stale-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := replaceExecutable(execPath, []byte("new-binary")); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, execPath, "new-binary")
	assertFileContents(t, execPath+".old", "current-binary")
}

func TestReplaceExecutableWindowsRestoresTargetWhenInstallFails(t *testing.T) {
	setReplaceTestGOOS(t, "windows")

	execPath := filepath.Join(t.TempDir(), "no-mistakes.exe")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	originalRename := renameFile
	renameCalls := 0
	renameFile = func(oldPath, newPath string) error {
		renameCalls++
		if renameCalls == 2 {
			return errors.New("injected install failure")
		}
		return os.Rename(oldPath, newPath)
	}
	t.Cleanup(func() { renameFile = originalRename })

	err := replaceExecutable(execPath, []byte("new-binary"))
	if err == nil || !strings.Contains(err.Error(), "injected install failure") {
		t.Fatalf("replaceExecutable error = %v", err)
	}
	assertFileContents(t, execPath, "old-binary")
	if _, err := os.Stat(execPath + ".old"); !os.IsNotExist(err) {
		t.Fatalf("backup should have been restored, stat err = %v", err)
	}
}

func TestCleanupOldExecutable(t *testing.T) {
	setReplaceTestGOOS(t, "windows")

	execPath := filepath.Join(t.TempDir(), "no-mistakes.exe")
	if err := os.WriteFile(execPath+".old", []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cleanupOldExecutable(execPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(execPath + ".old"); !os.IsNotExist(err) {
		t.Fatalf("backup should be removed, stat err = %v", err)
	}
	if err := cleanupOldExecutable(execPath); err != nil {
		t.Fatalf("absent backup should be a no-op: %v", err)
	}
}

func TestCleanupOldExecutableReturnsDeletionFailure(t *testing.T) {
	setReplaceTestGOOS(t, "windows")
	originalRemove := removeFile
	removeFile = func(string) error { return errors.New("executable still locked") }
	t.Cleanup(func() { removeFile = originalRemove })

	err := cleanupOldExecutable(`C:\bin\no-mistakes.exe`)
	if err == nil || !strings.Contains(err.Error(), "executable still locked") {
		t.Fatalf("cleanupOldExecutable error = %v", err)
	}
}

func TestReplaceExecutableNonWindowsUsesAtomicReplacement(t *testing.T) {
	setReplaceTestGOOS(t, "linux")

	execPath := filepath.Join(t.TempDir(), "no-mistakes")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0o751); err != nil {
		t.Fatal(err)
	}
	if err := replaceExecutable(execPath, []byte("new-binary")); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, execPath, "new-binary")
	if _, err := os.Stat(execPath + ".old"); !os.IsNotExist(err) {
		t.Fatalf("non-Windows replacement should not create a backup, stat err = %v", err)
	}
}

func TestReplaceExecutableNonWindowsFallsBackToOverwrite(t *testing.T) {
	setReplaceTestGOOS(t, "linux")

	execPath := filepath.Join(t.TempDir(), "no-mistakes")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0o751); err != nil {
		t.Fatal(err)
	}
	originalRename := renameFile
	renameFile = func(string, string) error { return errors.New("atomic rename unavailable") }
	t.Cleanup(func() { renameFile = originalRename })

	if err := replaceExecutable(execPath, []byte("new-binary")); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, execPath, "new-binary")
	info, err := os.Stat(execPath)
	if err != nil {
		t.Fatal(err)
	}
	wantPerm := os.FileMode(0o751)
	if runtime.GOOS == "windows" {
		wantPerm = 0o666
	}
	if got := info.Mode().Perm(); got != wantPerm {
		t.Fatalf("replacement permissions = %o, want %o", got, wantPerm)
	}
}

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

func setReplaceTestGOOS(t *testing.T, goos string) {
	t.Helper()
	original := currentGOOS
	currentGOOS = goos
	t.Cleanup(func() { currentGOOS = original })
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s contents = %q, want %q", path, got, want)
	}
}
