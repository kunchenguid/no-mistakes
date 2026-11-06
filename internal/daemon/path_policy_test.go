package daemon

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Re-exec isolates the real shellenv cache and process environment. The helper
// exercises the actual daemon startup preparation without starting a service.
func TestPrepareDaemonEnvironment_PathAugmentationOptOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not probe a login shell")
	}
	if os.Getenv("NM_PATH_POLICY_HELPER") == "1" {
		var log bytes.Buffer
		slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
		if err := prepareDaemonEnvironment(); err != nil {
			t.Fatal(err)
		}
		if got, want := os.Getenv("PATH"), os.Getenv("NM_FIXTURE_EXACT_PATH"); got != want {
			t.Fatalf("daemon PATH=%q, want %q", got, want)
		}
		if _, err := exec.LookPath("nm-fixture-path-tool"); !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("daemon resolved augmented tool: %v", err)
		}
		out, err := exec.Command("nm-fixture-chosen-tool").CombinedOutput()
		if err != nil || string(out) != "chosen" {
			t.Fatalf("chosen tool: %q, %v", out, err)
		}
		if !strings.Contains(log.String(), "shell_path_augmentation=false") {
			t.Fatalf("startup log omits opt-out: %s", &log)
		}
		return
	}
	root := t.TempDir()
	chosen := filepath.Join(root, "chosen")
	local := filepath.Join(root, ".local", "bin")
	for _, dir := range []string{chosen, local} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, script := range map[string]string{
		filepath.Join(chosen, "nm-fixture-chosen-tool"): "#!/bin/sh\nprintf chosen\n",
		filepath.Join(local, "nm-fixture-path-tool"):    "#!/bin/sh\nprintf wrong-install\n",
		filepath.Join(root, "bash"):                     "#!/bin/sh\nprintf 'PATH=%s\\000NO_MISTAKES_PATH_AUGMENT=0\\000' \"$NM_FIXTURE_EXACT_PATH\"\n",
	} {
		if err := os.WriteFile(path, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestPrepareDaemonEnvironment_PathAugmentationOptOut$")
	cmd.Env = []string{
		"HOME=" + root, "NM_HOME=" + filepath.Join(root, "nm"),
		"SHELL=" + filepath.Join(root, "bash"), "PATH=" + local,
		"NO_MISTAKES_TELEMETRY=0", "NO_MISTAKES_UPDATE_CHECK=0",
		"NM_PATH_POLICY_HELPER=1", "NM_FIXTURE_EXACT_PATH=" + chosen,
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("private daemon environment: %v\n%s", err, out)
	}
}
