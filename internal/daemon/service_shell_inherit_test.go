package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestReinstallSystemdServiceKeepsInstalledShellWhenResolutionDegrades is the
// regression guard for a PR #770 review finding: drift detection used to call
// resolveInstallShell() unconditionally on every `daemon start`. A daemon
// that installed successfully with a real SHELL (because the installing
// shell had $SHELL set, or getent/dscl resolved one) could later be
// restarted from a restricted environment where resolveInstallShell()
// degrades to the literal "bash" fallback (see shellenv.LoginShell) - the
// exact NixOS chicken-and-egg situation the baked-in SHELL fixes. Without
// this guard, that later degraded re-resolution would be treated as drift,
// overwrite the already-working absolute SHELL with the unusable "bash"
// literal, and reinstall+restart the daemon straight back into the PATH bug
// this mechanism exists to fix.
func TestReinstallSystemdServiceKeepsInstalledShellWhenResolutionDegrades(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "linux"
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceExecutablePath = func() (string, error) { return "/usr/local/bin/no-mistakes", nil }

	unitPath := filepath.Join(home, ".config", "systemd", "user", systemdServiceName(p))
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}

	// Install-time resolution succeeded with a real absolute shell.
	resolveInstallShell = func() string { return "/run/current-system/sw/bin/bash" }
	unit := renderSystemdUnit("/usr/local/bin/no-mistakes", p, home)
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}

	// A later `daemon start` runs in a restricted environment (no SHELL, no
	// reachable getent/bash) where the probe degrades to the literal "bash".
	resolveInstallShell = func() string { return "bash" }

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	daemonHealthCheck = func(*paths.Paths) (bool, error) { return true, nil }

	changed, err := reinstallManagedServiceIfChanged(p)
	if err != nil {
		t.Fatalf("reinstallManagedServiceIfChanged: %v", err)
	}
	if changed {
		t.Fatal("degraded shell re-resolution re-detected drift and reinstalled, reintroducing the NixOS PATH bug")
	}
	if len(commands) != 0 {
		t.Fatalf("no systemctl command should run when there is no real drift; ran %v", commands)
	}
	data, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != unit {
		t.Fatalf("unit changed after degraded-shell restart:\n%s", data)
	}
	if !strings.Contains(string(data), `Environment="SHELL=/run/current-system/sw/bin/bash"`) {
		t.Fatal("the previously-installed working SHELL was overwritten with the degraded fallback")
	}
}

// TestReinstallLaunchAgentKeepsInstalledShellWhenResolutionDegrades is the
// launchd counterpart.
func TestReinstallLaunchAgentKeepsInstalledShellWhenResolutionDegrades(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "darwin"
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceExecutablePath = func() (string, error) { return "/usr/local/bin/no-mistakes", nil }

	plistPath := launchAgentPath(p)
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatal(err)
	}

	resolveInstallShell = func() string { return "/opt/homebrew/bin/bash" }
	plist := renderLaunchAgent("/usr/local/bin/no-mistakes", p, home)
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}

	resolveInstallShell = func() string { return "bash" }

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	daemonHealthCheck = func(*paths.Paths) (bool, error) { return true, nil }

	changed, err := reinstallManagedServiceIfChanged(p)
	if err != nil {
		t.Fatalf("reinstallManagedServiceIfChanged: %v", err)
	}
	if changed {
		t.Fatal("degraded shell re-resolution re-detected drift and reinstalled, reintroducing the NixOS PATH bug")
	}
	if len(commands) != 0 {
		t.Fatalf("no launchctl command should run when there is no real drift; ran %v", commands)
	}
	data, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != plist {
		t.Fatalf("plist changed after degraded-shell restart:\n%s", data)
	}
	if !strings.Contains(string(data), "<key>SHELL</key>\n    <string>/opt/homebrew/bin/bash</string>") {
		t.Fatal("the previously-installed working SHELL was overwritten with the degraded fallback")
	}
}

// TestReinstallSystemdServiceAppliesAGenuineShellChange guards the other
// direction: the degraded-resolution guard above must not make drift
// detection blind to a real, intentional SHELL change (e.g. the installing
// user's default shell changed from bash to zsh). When resolveInstallShell
// returns a different absolute path, that is real drift and must still be
// applied.
func TestReinstallSystemdServiceAppliesAGenuineShellChange(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "linux"
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceExecutablePath = func() (string, error) { return "/usr/local/bin/no-mistakes", nil }

	unitPath := filepath.Join(home, ".config", "systemd", "user", systemdServiceName(p))
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}

	resolveInstallShell = func() string { return "/bin/bash" }
	unit := renderSystemdUnit("/usr/local/bin/no-mistakes", p, home)
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}

	resolveInstallShell = func() string { return "/usr/bin/zsh" }

	running := true
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		command := name + " " + strings.Join(args, " ")
		if strings.Contains(command, "systemctl --user stop ") {
			running = false
		}
		if strings.Contains(command, "systemctl --user restart ") || strings.Contains(command, "systemctl --user start ") {
			running = true
		}
		return nil, nil
	}
	daemonHealthCheck = func(*paths.Paths) (bool, error) { return running, nil }

	changed, err := reinstallManagedServiceIfChanged(p)
	if err != nil {
		t.Fatalf("reinstallManagedServiceIfChanged: %v", err)
	}
	if !changed {
		t.Fatal("a genuine SHELL change should still be detected as drift and applied")
	}
	data, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `Environment="SHELL=/usr/bin/zsh"`) {
		t.Fatalf("expected the new SHELL to be applied, got:\n%s", data)
	}
}
