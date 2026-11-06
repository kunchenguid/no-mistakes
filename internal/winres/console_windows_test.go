package winres

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// consoleAllocationPolicyBuild is the first Windows build that honors the
// manifest's consoleAllocationPolicy (Windows 11 24H2, Server 2025).
const consoleAllocationPolicyBuild = 26100

// Issue #1653: Task Scheduler starts the managed daemon without a console to
// inherit, so Windows used to allocate a visible console window for the
// console-subsystem binary. The manifest's detached policy must leave the real
// no-mistakes.exe console-less when a console-less parent starts it without
// creation flags. The probe observes that from outside, so the product needs
// no test hook.
func TestNoMistakesGetsNoConsoleFromAConsolelessParent(t *testing.T) {
	if testing.Short() {
		t.Skip("builds no-mistakes.exe and a GUI-subsystem probe")
	}
	if _, _, build := windows.RtlGetNtVersionNumbers(); build < consoleAllocationPolicyBuild {
		t.Skipf("Windows build %d ignores consoleAllocationPolicy (needs %d, Windows 11 24H2 / Server 2025)", build, consoleAllocationPolicyBuild)
	}
	exe := buildNoMistakes(t, runtime.GOARCH)
	probe := buildConsoleProbe(t)

	// Control: a console-subsystem binary without the manifest (this test
	// binary) does get a console from the same parent, so the probe can see
	// one. -hide keeps that console's window from flashing.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got := runConsoleProbe(t, probe, []string{"NM_WINRES_TEST_HELPER=ready"}, "-hide", self); got != "console" {
		t.Fatalf("control without the manifest: probe = %q, want console", got)
	}

	// `daemon log-sink` is a no-mistakes entrypoint that reports readiness
	// and blocks on stdin, without daemon run's own console release.
	if got := runConsoleProbe(t, probe, nil, exe, "daemon", "log-sink", "--root", t.TempDir()); got != "no-console" {
		t.Fatalf("no-mistakes.exe from a console-less parent: probe = %q, want no-console", got)
	}
}

func buildConsoleProbe(t *testing.T) string {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "consoleprobe.exe")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-ldflags=-H=windowsgui", "-o", probe, "./testdata/consoleprobe")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build console probe: %v\n%s", err, out)
	}
	return probe
}

func runConsoleProbe(t *testing.T, probe string, env []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, probe, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("console probe %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
