package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Re-exec isolates the real shellenv cache and process environment. The helper
// exercises the actual daemon startup preparation without starting a service.
func TestPrepareDaemonEnvironment_PathAugmentationOptOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not probe a login shell")
	}
	if os.Getenv("NM_PATH_POLICY_HELPER") == "1" {
		for range 2 {
			if err := prepareDaemonEnvironment(); err != nil {
				t.Fatal(err)
			}
			if got, want := os.Getenv("PATH"), os.Getenv("NM_FIXTURE_WANT_PATH"); got != want {
				t.Fatalf("daemon PATH=%q, want %q", got, want)
			}
			tool, err := exec.LookPath("nm-fixture-path-tool")
			if os.Getenv("NM_FIXTURE_OPT_OUT") == "1" {
				if !errors.Is(err, exec.ErrNotFound) {
					t.Fatalf("daemon resolved augmented tool: %q, %v", tool, err)
				}
			} else if want := filepath.Join(os.Getenv("HOME"), ".local", "bin", "nm-fixture-path-tool"); err != nil || tool != want {
				t.Fatalf("default tool lookup: %q, %v; want %q", tool, err, want)
			}
			out, err := exec.Command("nm-fixture-chosen-tool").CombinedOutput()
			if err != nil || string(out) != "chosen" {
				t.Fatalf("chosen tool: %q, %v", out, err)
			}
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
		filepath.Join(root, "bash"):                     "#!/bin/sh\nprintf 'PATH=%s:%s\\000NO_MISTAKES_PATH_AUGMENT=%s\\000' \"$NM_FIXTURE_CHOSEN\" \"$PATH\" \"$NM_FIXTURE_SHELL_CONTROL\"\n",
	} {
		if err := os.WriteFile(path, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, launcher, shellControl string
		optOut                       bool
	}{
		{name: "default"},
		{name: "enabled launcher wins", launcher: "1", shellControl: "0"},
		{name: "unknown launcher wins", launcher: "other", shellControl: "0"},
		{name: "shell opt out", shellControl: "0", optOut: true},
		{name: "launcher opt out wins", launcher: "0", shellControl: "1", optOut: true},
		{name: "launcher and shell opt out", launcher: "0", shellControl: "0", optOut: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantPath := chosen + ":" + managedServicePath(root)
			optOut := "0"
			if tc.optOut {
				wantPath = chosen + ":/usr/bin:/bin"
				optOut = "1"
			}
			cmd := exec.Command(exe, "-test.run=^TestPrepareDaemonEnvironment_PathAugmentationOptOut$")
			cmd.Env = []string{
				"HOME=" + root, "NM_HOME=" + filepath.Join(root, "nm"),
				"SHELL=" + filepath.Join(root, "bash"), "PATH=" + managedServicePath(root),
				"NO_MISTAKES_TELEMETRY=0", "NO_MISTAKES_UPDATE_CHECK=0",
				"NO_MISTAKES_PATH_AUGMENT=" + tc.launcher,
				"NM_PATH_POLICY_HELPER=1", "NM_FIXTURE_CHOSEN=" + chosen,
				"NM_FIXTURE_SHELL_CONTROL=" + tc.shellControl,
				"NM_FIXTURE_WANT_PATH=" + wantPath, "NM_FIXTURE_OPT_OUT=" + optOut,
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("private daemon environment: %v\n%s", err, out)
			}
		})
	}
}
