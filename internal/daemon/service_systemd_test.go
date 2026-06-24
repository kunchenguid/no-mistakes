package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestStartInstallsSystemdUnitAndStartsManagedDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("systemd unit rendering depends on POSIX path formatting")
	}
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

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	checks := 0
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		checks++
		return checks >= 2, nil
	}

	if err := Start(p); err != nil {
		t.Fatal(err)
	}

	unitPath := filepath.Join(home, ".config", "systemd", "user", systemdServiceName(p))
	data, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"Description=no-mistakes background daemon",
		"ExecStart=/usr/local/bin/no-mistakes daemon run --root " + p.Root(),
		"WorkingDirectory=" + p.Root(),
		"Environment=\"HOME=" + home + "\"",
		"Restart=always",
		"WantedBy=default.target",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("systemd unit should contain %q, got:\n%s", want, text)
		}
	}
	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable " + systemdServiceName(p),
		"systemctl --user start " + systemdServiceName(p),
	}
	if len(commands) != len(want) {
		t.Fatalf("expected %d systemctl commands, got %v", len(want), commands)
	}
	for i, wantCmd := range want {
		if commands[i] != wantCmd {
			t.Fatalf("command[%d] = %q, want %q", i, commands[i], wantCmd)
		}
	}
}

func TestInstallSystemdUserServiceDoesNotRemoveLegacyUnitForDifferentRoot(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "linux"
	serviceUserHomeDir = func() (string, error) { return home, nil }

	legacyPath := filepath.Join(home, ".config", "systemd", "user", legacySystemdServiceName)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	otherRoot := filepath.Join(t.TempDir(), "other-nm-home")
	legacyUnit := renderSystemdUnit("/usr/local/bin/no-mistakes", paths.WithRoot(otherRoot), home)
	if err := os.WriteFile(legacyPath, []byte(legacyUnit), 0o644); err != nil {
		t.Fatal(err)
	}

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, nil
	}

	if err := installSystemdUserService(p, "/usr/local/bin/no-mistakes"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy unit for different root should remain: %v", err)
	}
	if len(commands) != 2 {
		t.Fatalf("install should not stop unrelated legacy service, got commands %v", commands)
	}
}

func TestInstallSystemdUserServiceKeepsLegacyUnitOnEnableFailure(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "linux"
	serviceUserHomeDir = func() (string, error) { return home, nil }

	legacyPath := filepath.Join(home, ".config", "systemd", "user", legacySystemdServiceName)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		command := name + " " + strings.Join(args, " ")
		commands = append(commands, command)
		if command == "systemctl --user enable "+systemdServiceName(p) {
			return nil, fmt.Errorf("enable failed")
		}
		return nil, nil
	}

	err := installSystemdUserService(p, "/usr/local/bin/no-mistakes")
	if err == nil {
		t.Fatal("installSystemdUserService should fail when enable fails")
	}
	if _, statErr := os.Stat(legacyPath); statErr != nil {
		t.Fatalf("legacy unit should remain after failed scoped install: %v", statErr)
	}
	for _, command := range commands {
		if strings.Contains(command, "--user disable "+legacySystemdServiceName) || strings.Contains(command, "--user stop "+legacySystemdServiceName) {
			t.Fatalf("legacy cleanup should not run before successful scoped install, got %q", command)
		}
	}
}

func TestServiceProxyEnvSkipsUnsetAndEmpty(t *testing.T) {
	for _, key := range proxyEnvKeys {
		t.Setenv(key, "")
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7897")

	got := serviceProxyEnv()
	if len(got) != 1 || got[0][0] != "HTTPS_PROXY" || got[0][1] != "http://127.0.0.1:7897" {
		t.Fatalf("serviceProxyEnv() = %v, want a single HTTPS_PROXY entry", got)
	}
}

func TestRenderSystemdUnitForwardsProxyEnv(t *testing.T) {
	for _, key := range proxyEnvKeys {
		t.Setenv(key, "")
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7897")
	t.Setenv("NO_PROXY", "localhost,127.0.0.1")

	unit := renderSystemdUnit("/usr/local/bin/no-mistakes", paths.WithRoot(t.TempDir()), "/home/u")
	for _, want := range []string{
		`Environment="HTTPS_PROXY=http://127.0.0.1:7897"`,
		`Environment="NO_PROXY=localhost,127.0.0.1"`,
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("systemd unit should forward proxy env %q, got:\n%s", want, unit)
		}
	}
}

func TestWriteServiceFileTightensModeWhenProxyPresent(t *testing.T) {
	for _, key := range proxyEnvKeys {
		t.Setenv(key, "")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "unit")

	// No proxy: the conventional 0644 is kept.
	if err := writeServiceFile(path, "no-proxy"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("without proxy, mode = %o, want 0644", got)
	}

	// Proxy present: re-installing over the existing 0644 file must tighten it
	// to owner-only 0600 so forwarded credentials are not world-readable.
	t.Setenv("HTTPS_PROXY", "http://user:pass@127.0.0.1:7897")
	if err := writeServiceFile(path, "with-proxy"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("with proxy, mode = %o, want 0600", got)
	}
}
