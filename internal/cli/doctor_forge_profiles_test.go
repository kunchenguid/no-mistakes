package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

func TestDoctorValidatesForgeProfileWithItsAuthoritativeEnvironment(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	profileDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(profileDir, "hosts.yml"), []byte("github.com:\n    user: personal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	global := fmt.Sprintf("agent: codex\nforge_profiles:\n  github.com:\n    gh_config_dir: %s\n", profileDir)
	if err := os.WriteFile(filepath.Join(nmHome, "config.yaml"), []byte(global), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	writeDoctorStubBinary(t, binDir, "codex")
	writeDoctorProfileGHBinary(t, binDir, profileDir)
	t.Setenv("NM_HOME", nmHome)
	t.Setenv("PATH", binDir)
	t.Setenv("GH_TOKEN", "must-not-leak")

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "forge github.com") || !strings.Contains(out, "authenticated") {
		t.Fatalf("doctor did not report the validated forge profile:\n%s", out)
	}
}

func TestDoctorReportsForgeProfileAuthenticationFailureWithoutChangingExitContract(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	profileDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(profileDir, "hosts.yml"), []byte("github.com:\n    user: personal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	global := fmt.Sprintf("agent: codex\nforge_profiles:\n  github.com:\n    gh_config_dir: %s\n", profileDir)
	if err := os.WriteFile(filepath.Join(nmHome, "config.yaml"), []byte(global), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	writeDoctorStubBinary(t, binDir, "codex")
	writeDoctorFailingBinary(t, binDir, "gh")
	t.Setenv("NM_HOME", nmHome)
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor changed its zero-exit reporting contract: %v\n%s", err, out)
	}
	for _, want := range []string{"forge github.com", "authentication failed", "some checks failed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorValidatesGitLabForgeProfile(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	profileDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(profileDir, "config.yml"), []byte("hosts:\n    gitlab.com:\n        user: work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	global := fmt.Sprintf("agent: codex\nforge_profiles:\n  gitlab.com:\n    glab_config_dir: %s\n", profileDir)
	if err := os.WriteFile(filepath.Join(nmHome, "config.yaml"), []byte(global), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	writeDoctorStubBinary(t, binDir, "codex")
	writeDoctorStubBinary(t, binDir, "glab")
	t.Setenv("NM_HOME", nmHome)
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "forge gitlab.com") || !strings.Contains(out, "gitlab authenticated") {
		t.Fatalf("doctor did not report GitLab profile success:\n%s", out)
	}
}

func writeDoctorProfileGHBinary(t *testing.T, dir, profileDir string) {
	t.Helper()
	name := "gh"
	contents := fmt.Sprintf("#!/bin/sh\n[ \"$GH_CONFIG_DIR\" = %q ] || exit 10\n[ -z \"$GH_TOKEN\" ] || exit 11\nexit 0\n", profileDir)
	if runtime.GOOS == "windows" {
		name = "gh.cmd"
		contents = fmt.Sprintf("@echo off\r\nif not \"%%GH_CONFIG_DIR%%\"==\"%s\" exit /b 10\r\nif defined GH_TOKEN exit /b 11\r\nexit /b 0\r\n", profileDir)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeDoctorFailingBinary(t *testing.T, dir, base string) {
	t.Helper()
	name := base
	contents := "#!/bin/sh\necho invalid-auth >&2\nexit 1\n"
	if runtime.GOOS == "windows" {
		name += ".cmd"
		contents = "@echo off\r\necho invalid-auth 1>&2\r\nexit /b 1\r\n"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}
