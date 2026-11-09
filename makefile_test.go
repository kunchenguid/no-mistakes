package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The fork's build contract: a local-only flow with the version taken from
// git and no telemetry configuration embedded anywhere. This asserts the
// Makefile never embeds telemetry settings (empty config = zero network
// requests) and that a target platform can be selected.
func TestMakeBuildEmbedsNoTelemetryAndHonorsTargetPlatform(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("make build tests are POSIX-oriented")
	}
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}

	workDir := t.TempDir()
	data, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "Makefile"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	dryRun := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(makePath, append([]string{"-n"}, args...)...)
		cmd.Dir = workDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("make -n %v failed: %v\n%s", args, err, out)
		}
		return string(out)
	}

	hostOut := dryRun("build")
	for _, forbid := range []string{"TelemetryHost", "TelemetryWebsiteID", "UMAMI", "a.kunchenguid.com"} {
		if strings.Contains(hostOut, forbid) {
			t.Fatalf("make build must not embed %q, got:\n%s", forbid, hostOut)
		}
	}
	if !strings.Contains(hostOut, "buildinfo.Version=") {
		t.Fatalf("make build should embed the version, got:\n%s", hostOut)
	}
	if !strings.Contains(hostOut, "-o bin/no-mistakes ") {
		t.Fatalf("make build should target the host binary name, got:\n%s", hostOut)
	}

	winOut := dryRun("build", "GOOS=windows", "GOARCH=amd64")
	if !strings.Contains(winOut, "-o bin/no-mistakes.exe ") {
		t.Fatalf("cross-build should use the Windows binary name, got:\n%s", winOut)
	}
}
