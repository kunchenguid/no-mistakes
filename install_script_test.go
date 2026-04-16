package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestInstallScriptInstallsUserOwnedBinaryAndPathSymlink(t *testing.T) {
	skipInstallScriptTestsOnWindows(t)

	home := t.TempDir()
	archivePath := filepath.Join(t.TempDir(), "no-mistakes-v1.2.3-darwin-arm64.tar.gz")
	binaryScript := "#!/bin/sh\nexit 0\n"
	makeInstallArchive(t, archivePath, binaryScript)
	fakeBin := makeFakeInstallCommands(t)
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}

	runInstallScript(t, home, fakeBin, map[string]string{
		"FAKE_RELEASE_ARCHIVE": archivePath,
	})

	realBin := filepath.Join(home, ".no-mistakes", "bin", "no-mistakes")
	assertFileContent(t, realBin, binaryScript)
	assertSymlinkTarget(t, filepath.Join(localBin, "no-mistakes"), realBin)
}

func TestInstallScriptReplacesExistingPathEntryWithSymlink(t *testing.T) {
	skipInstallScriptTestsOnWindows(t)

	home := t.TempDir()
	archivePath := filepath.Join(t.TempDir(), "no-mistakes-v1.2.3-darwin-arm64.tar.gz")
	binaryScript := "#!/bin/sh\nexit 0\n"
	makeInstallArchive(t, archivePath, binaryScript)
	fakeBin := makeFakeInstallCommands(t)
	linkDir := filepath.Join(t.TempDir(), "link-bin")
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(linkDir, "no-mistakes")
	if err := os.WriteFile(oldPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	runInstallScript(t, home, fakeBin, map[string]string{
		"FAKE_RELEASE_ARCHIVE": archivePath,
		"NO_MISTAKES_LINK_DIR": linkDir,
	})

	realBin := filepath.Join(home, ".no-mistakes", "bin", "no-mistakes")
	assertFileContent(t, realBin, binaryScript)
	assertSymlinkTarget(t, oldPath, realBin)
}

func TestInstallScriptStartsDaemonAfterInstall(t *testing.T) {
	skipInstallScriptTestsOnWindows(t)

	home := t.TempDir()
	archivePath := filepath.Join(t.TempDir(), "no-mistakes-v1.2.3-darwin-arm64.tar.gz")
	callLog := filepath.Join(t.TempDir(), "calls.log")
	makeInstallArchive(t, archivePath, "#!/bin/sh\nprintf '%s\n' \"$*\" >> \"$NO_MISTAKES_CALL_LOG\"\n")
	fakeBin := makeFakeInstallCommands(t)
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}

	runInstallScript(t, home, fakeBin, map[string]string{
		"FAKE_RELEASE_ARCHIVE": archivePath,
		"NO_MISTAKES_CALL_LOG": callLog,
	})

	data, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "daemon start") {
		t.Fatalf("install.sh should start the daemon after install, got calls %q", string(data))
	}
}

func TestInstallScriptSucceedsWhenDaemonStartFails(t *testing.T) {
	skipInstallScriptTestsOnWindows(t)

	home := t.TempDir()
	archivePath := filepath.Join(t.TempDir(), "no-mistakes-v1.2.3-darwin-arm64.tar.gz")
	callLog := filepath.Join(t.TempDir(), "calls.log")
	makeInstallArchive(t, archivePath, "#!/bin/sh\nprintf '%s\n' \"$*\" >> \"$NO_MISTAKES_CALL_LOG\"\nif [ \"$1\" = \"daemon\" ] && [ \"$2\" = \"start\" ]; then\n  exit 23\nfi\n")
	fakeBin := makeFakeInstallCommands(t)
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}

	output, err := runInstallScriptCommand(t, home, fakeBin, map[string]string{
		"FAKE_RELEASE_ARCHIVE": archivePath,
		"NO_MISTAKES_CALL_LOG": callLog,
	})
	if err != nil {
		t.Fatalf("install.sh should succeed even when daemon start fails: %v\n%s", err, output)
	}

	data, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "daemon start") {
		t.Fatalf("install.sh should still attempt daemon start, got calls %q", string(data))
	}
}

func skipInstallScriptTestsOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is a POSIX installer; Windows uses install.ps1")
	}
}

func runInstallScript(t *testing.T, home, fakeBin string, extraEnv map[string]string) {
	t.Helper()
	output, err := runInstallScriptCommand(t, home, fakeBin, extraEnv)
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, output)
	}
}

func runInstallScriptCommand(t *testing.T, home, fakeBin string, extraEnv map[string]string) ([]byte, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "docs/install.sh")
	pathValue := strings.Join([]string{fakeBin, filepath.Join(home, ".local", "bin"), os.Getenv("PATH")}, string(os.PathListSeparator))
	cmd.Env = append(filteredEnv(os.Environ(), "HOME", "PATH"), []string{
		"HOME=" + home,
		"PATH=" + pathValue,
	}...)
	for key, value := range extraEnv {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	return cmd.CombinedOutput()
}

func filteredEnv(env []string, excluded ...string) []string {
	blocked := make(map[string]struct{}, len(excluded))
	for _, key := range excluded {
		blocked[key] = struct{}{}
	}
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			filtered = append(filtered, entry)
			continue
		}
		if _, skip := blocked[key]; skip {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func makeInstallArchive(t *testing.T, archivePath, binaryContent string) {
	t.Helper()

	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	data := []byte(binaryContent)
	hdr := &tar.Header{Name: "no-mistakes", Mode: 0o755, Size: int64(len(data))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func makeFakeInstallCommands(t *testing.T) string {
	t.Helper()

	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "uname"), `#!/bin/sh
case "$1" in
  -s) printf 'Darwin\n' ;;
  -m) printf 'arm64\n' ;;
  *) command uname "$@" ;;
esac
`)
	writeExecutable(t, filepath.Join(binDir, "curl"), `#!/bin/sh
out=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
if [ -n "$out" ]; then
  cp "$FAKE_RELEASE_ARCHIVE" "$out"
  exit 0
fi
	printf '{"tag_name":"v1.2.3"}'
`)
	writeExecutable(t, filepath.Join(binDir, "sudo"), "#!/bin/sh\nexec \"$@\"\n")
	return binDir
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("file %s = %q, want %q", path, string(data), want)
	}
}

func assertSymlinkTarget(t *testing.T, path, wantTarget string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", path)
	}
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	if target != wantTarget {
		t.Fatalf("symlink %s -> %s, want %s", path, target, wantTarget)
	}
}
