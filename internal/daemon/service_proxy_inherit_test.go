package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestReinstallSystemdServiceInheritsProxyFromExistingUnitWhenEnvUnset is the
// core regression guard for the drift-detection proxy strip (#322 review
// finding #1). After a proxy-time install the on-disk unit carries the proxy
// Environment= lines. A later `daemon start` from a shell that does NOT export
// the proxy variables must not re-detect drift and reinstall: doing so would
// strip the baked-in proxy and re-break the daemon with "403 Request not
// allowed". The render used to compute the drift target now inherits the
// already-baked proxy from the existing unit (mirroring the existing executable
// inheritance), so the target matches the on-disk unit and no reinstall runs.
func TestReinstallSystemdServiceInheritsProxyFromExistingUnitWhenEnvUnset(t *testing.T) {
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

	// Render and persist the exact unit a proxy-time install produces, including
	// a percent-encoded credential whose % the renderer doubled.
	for _, key := range proxyEnvKeys {
		t.Setenv(key, "")
	}
	t.Setenv("HTTPS_PROXY", "http://user:p%40ss@127.0.0.1:7897")
	unit := renderSystemdUnit("/usr/local/bin/no-mistakes", p, home)
	if err := os.WriteFile(unitPath, []byte(unit), 0o600); err != nil {
		t.Fatal(err)
	}

	// The daemon now restarts from a shell WITHOUT the proxy exported.
	for _, key := range proxyEnvKeys {
		t.Setenv(key, "")
	}

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
		t.Fatal("env-less restart re-detected drift and reinstalled, which strips the baked-in proxy")
	}
	if len(commands) != 0 {
		t.Fatalf("no systemctl command should run when there is no drift; ran %v", commands)
	}
	data, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != unit {
		t.Fatalf("unit changed after env-less restart:\n%s", data)
	}
	if !strings.Contains(string(data), `Environment="HTTPS_PROXY=`) {
		t.Fatal("forwarded proxy was stripped from the unit on env-less restart")
	}
}

// TestReinstallLaunchAgentInheritsProxyFromExistingPlistWhenEnvUnset is the
// launchd counterpart of the drift no-op guard.
func TestReinstallLaunchAgentInheritsProxyFromExistingPlistWhenEnvUnset(t *testing.T) {
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

	for _, key := range proxyEnvKeys {
		t.Setenv(key, "")
	}
	t.Setenv("HTTPS_PROXY", "http://user:pass@127.0.0.1:7897")
	plist := renderLaunchAgent("/usr/local/bin/no-mistakes", p, home)
	if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, key := range proxyEnvKeys {
		t.Setenv(key, "")
	}

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
		t.Fatal("env-less restart re-detected drift and reinstalled, which strips the baked-in proxy")
	}
	if len(commands) != 0 {
		t.Fatalf("no launchctl command should run when there is no drift; ran %v", commands)
	}
	data, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != plist {
		t.Fatalf("plist changed after env-less restart:\n%s", data)
	}
	if !strings.Contains(string(data), "<key>HTTPS_PROXY</key>") {
		t.Fatal("forwarded proxy was stripped from the plist on env-less restart")
	}
}

// TestInstallSystemdUserServiceInheritsProxyFromExistingWhenEnvUnset guards the
// write path: when a reinstall is legitimately triggered (e.g. the binary path
// changed) from a shell without the proxy exported, the freshly written unit
// must still carry the proxy inherited from the prior on-disk unit and keep the
// owner-only 0600 mode, rather than stripping the credentials.
func TestInstallSystemdUserServiceInheritsProxyFromExistingWhenEnvUnset(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes (0600) are not enforced on Windows; the proxy-bearing service file is only generated on macOS/Linux")
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
	serviceCommandRunner = func(string, ...string) ([]byte, error) { return nil, nil }

	unitPath := filepath.Join(home, ".config", "systemd", "user", systemdServiceName(p))
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, key := range proxyEnvKeys {
		t.Setenv(key, "")
	}
	t.Setenv("HTTPS_PROXY", "http://user:pass@127.0.0.1:7897")
	if err := os.WriteFile(unitPath, []byte(renderSystemdUnit("/old/no-mistakes", p, home)), 0o600); err != nil {
		t.Fatal(err)
	}

	// Reinstall from a shell without the proxy exported.
	for _, key := range proxyEnvKeys {
		t.Setenv(key, "")
	}
	if err := installSystemdUserService(p, "/usr/local/bin/no-mistakes"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `Environment="HTTPS_PROXY=http://user:pass@127.0.0.1:7897"`) {
		t.Fatalf("reinstall stripped the inherited proxy:\n%s", data)
	}
	if !strings.Contains(string(data), "ExecStart=/usr/local/bin/no-mistakes") {
		t.Fatalf("reinstall should update the executable:\n%s", data)
	}
	if info, err := os.Stat(unitPath); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("inherited-proxy unit mode = %o, want 0600", info.Mode().Perm())
	}
}
