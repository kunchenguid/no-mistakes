package daemon

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestStartInstallsLaunchAgentAndBootstrapsManagedDaemon(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "darwin"
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceCurrentUser = func() (*user.User, error) { return &user.User{Uid: "501"}, nil }
	serviceExecutablePath = func() (string, error) { return "/opt/no-mistakes/bin/no-mistakes", nil }

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

	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdServiceLabel(p)+".plist")
	data, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"<string>/opt/no-mistakes/bin/no-mistakes</string>",
		"<string>daemon</string>",
		"<string>run</string>",
		"<string>--root</string>",
		"<string>" + p.Root() + "</string>",
		"<key>EnvironmentVariables</key>",
		"<key>HOME</key>",
		"<string>" + home + "</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("launch agent should contain %q, got:\n%s", want, text)
		}
	}
	if len(commands) != 3 {
		t.Fatalf("expected bootout, bootstrap, and kickstart, got %v", commands)
	}
	if want := "launchctl bootout gui/501/" + launchdServiceLabel(p); commands[0] != want {
		t.Fatalf("bootout command = %q, want %q", commands[0], want)
	}
	if want := "launchctl bootstrap gui/501 " + plistPath; commands[1] != want {
		t.Fatalf("bootstrap command = %q, want %q", commands[1], want)
	}
	if want := "launchctl kickstart -k gui/501/" + launchdServiceLabel(p); commands[2] != want {
		t.Fatalf("kickstart command = %q, want %q", commands[2], want)
	}
}

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

func TestStartInstallsWindowsTaskAndStartsManagedDaemon_EnableWindowsCI(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"
	exe := `C:\Program Files\no-mistakes\no-mistakes.exe`
	serviceExecutablePath = func() (string, error) { return exe, nil }

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		// Simulate fresh-install: the legacy unsuffixed task is absent, so
		// the pre-install cleanup query fails and cleanupLegacyWindowsTask
		// returns without issuing End/Delete.
		if name == "schtasks" && len(args) >= 3 && args[0] == "/Query" && args[2] == legacyWindowsTaskName {
			return nil, fmt.Errorf("task not found")
		}
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

	wantTaskCommand := strconv.Quote(exe) + " daemon run --root " + strconv.Quote(p.Root())
	wantQueryLegacy := "schtasks /Query /TN " + legacyWindowsTaskName
	wantCreate := "schtasks /Create /TN " + windowsTaskName(p) +
		" /SC ONLOGON /RL LIMITED /F /TR " + wantTaskCommand
	wantRun := "schtasks /Run /TN " + windowsTaskName(p)
	if len(commands) != 3 {
		t.Fatalf("expected schtasks create, legacy query, and run, got %v", commands)
	}
	if commands[0] != wantCreate {
		t.Fatalf("create command = %q, want %q", commands[0], wantCreate)
	}
	if commands[1] != wantQueryLegacy {
		t.Fatalf("legacy query command = %q, want %q", commands[1], wantQueryLegacy)
	}
	if commands[2] != wantRun {
		t.Fatalf("run command = %q, want %q", commands[2], wantRun)
	}
}

func TestInstallLaunchAgentKeepsLegacyPlistOnScopedWriteFailure(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "darwin"
	serviceUserHomeDir = func() (string, error) { return home, nil }

	legacyPath := filepath.Join(home, "Library", "LaunchAgents", legacyLaunchdServiceLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(launchAgentPath(p), 0o755); err != nil {
		t.Fatal(err)
	}

	err := installLaunchAgent(p, "/opt/no-mistakes/bin/no-mistakes")
	if err == nil {
		t.Fatal("installLaunchAgent should fail when scoped plist path is a directory")
	}
	if _, statErr := os.Stat(legacyPath); statErr != nil {
		t.Fatalf("legacy plist should remain after failed scoped install: %v", statErr)
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

func TestInstallWindowsTaskKeepsLegacyTaskOnCreateFailure_EnableWindowsCI(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		command := name + " " + strings.Join(args, " ")
		commands = append(commands, command)
		if name == "schtasks" && len(args) > 0 && args[0] == "/Create" {
			return nil, fmt.Errorf("create failed")
		}
		return nil, nil
	}

	err := installWindowsTask(p, `C:\Program Files\no-mistakes\no-mistakes.exe`)
	if err == nil {
		t.Fatal("installWindowsTask should fail when schtasks create fails")
	}
	for _, command := range commands {
		if strings.Contains(command, "/End /TN "+legacyWindowsTaskName) || strings.Contains(command, "/Delete /TN "+legacyWindowsTaskName+" /F") {
			t.Fatalf("legacy cleanup should not run before successful scoped install, got %q", command)
		}
	}
}

func TestStartFallsBackToDetachedDaemonWhenManagedStartFails(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	t.Setenv("NM_DAEMON_HELPER_PROCESS", "1")
	runtimeGOOS = "linux"
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceExecutablePath = func() (string, error) { return "/usr/local/bin/no-mistakes", nil }

	var commands []string
	var managedStopped bool
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		command := name + " " + strings.Join(args, " ")
		commands = append(commands, command)
		if command == "systemctl --user stop "+systemdServiceName(p) {
			managedStopped = true
			return nil, nil
		}
		if command == "systemctl --user start "+systemdServiceName(p) {
			return nil, fmt.Errorf("user manager unavailable")
		}
		return nil, nil
	}
	checks := 0
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		checks++
		return checks >= 3, nil
	}

	if err := Start(p); err != nil {
		t.Fatalf("Start should fall back to detached mode: %v", err)
	}

	if len(commands) != 4 {
		t.Fatalf("expected managed start, stop, and detached fallback, got %v", commands)
	}
	if want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable " + systemdServiceName(p),
		"systemctl --user start " + systemdServiceName(p),
		"systemctl --user stop " + systemdServiceName(p),
	}; len(commands) == len(want) {
		for i, wantCmd := range want {
			if commands[i] != wantCmd {
				t.Fatalf("command[%d] = %q, want %q", i, commands[i], wantCmd)
			}
		}
	}
	if !managedStopped {
		t.Fatal("managed service should be stopped before detached fallback")
	}
	if _, err := os.Stat(p.DaemonLog()); err != nil {
		t.Fatalf("detached fallback should open daemon log: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user", systemdServiceName(p))); err != nil {
		t.Fatalf("managed service install should still write unit file: %v", err)
	}
	if pidData, err := os.ReadFile(p.PIDFile()); err == nil && len(pidData) > 0 {
		t.Fatalf("helper detached process should not leave a pid file, got %q", string(pidData))
	}
	if checks < 3 {
		t.Fatalf("expected health checks for preflight, managed failure, and detached wait, got %d", checks)
	}
	_ = os.Remove(p.DaemonLog())
	_ = os.Remove(p.PIDFile())
	_ = os.Remove(p.Socket())
}

func TestStartStopsManagedServiceBeforeDetachedFallbackAfterTimeout(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	t.Setenv("NM_DAEMON_HELPER_PROCESS", "1")
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "20ms")
	t.Setenv("NM_TEST_DAEMON_START_POLL_INTERVAL", "1ms")
	runtimeGOOS = "linux"
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceExecutablePath = func() (string, error) { return "/usr/local/bin/no-mistakes", nil }

	var commands []string
	var managedStopped bool
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		command := name + " " + strings.Join(args, " ")
		commands = append(commands, command)
		if command == "systemctl --user stop "+systemdServiceName(p) {
			managedStopped = true
		}
		return nil, nil
	}
	checks := 0
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		checks++
		if !managedStopped {
			return false, nil
		}
		return checks > 2, nil
	}

	if err := Start(p); err != nil {
		t.Fatalf("Start should fall back to detached mode after managed timeout: %v", err)
	}

	if len(commands) != 4 {
		t.Fatalf("expected managed start, stop, and detached fallback, got %v", commands)
	}
	if want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable " + systemdServiceName(p),
		"systemctl --user start " + systemdServiceName(p),
		"systemctl --user stop " + systemdServiceName(p),
	}; len(commands) == len(want) {
		for i, wantCmd := range want {
			if commands[i] != wantCmd {
				t.Fatalf("command[%d] = %q, want %q", i, commands[i], wantCmd)
			}
		}
	}
	if !managedStopped {
		t.Fatal("managed service should be stopped before detached fallback")
	}
	if _, err := os.Stat(p.DaemonLog()); err != nil {
		t.Fatalf("detached fallback should open daemon log: %v", err)
	}
	if checks < 3 {
		t.Fatalf("expected health checks during managed timeout and detached wait, got %d", checks)
	}
	_ = os.Remove(p.DaemonLog())
	_ = os.Remove(p.PIDFile())
	_ = os.Remove(p.Socket())
}

func TestStopUsesManagedServiceWhenInstalled(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "linux"
	serviceUserHomeDir = func() (string, error) { return home, nil }
	daemonHealthCheck = func(*paths.Paths) (bool, error) { return false, nil }

	unitPath := filepath.Join(home, ".config", "systemd", "user", systemdServiceName(p))
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, nil
	}

	if err := Stop(p); err != nil {
		t.Fatal(err)
	}

	if len(commands) != 1 {
		t.Fatalf("expected one stop command, got %v", commands)
	}
	if want := "systemctl --user stop " + systemdServiceName(p); commands[0] != want {
		t.Fatalf("stop command = %q, want %q", commands[0], want)
	}
}

func TestStopFallsBackToDetachedDaemonWhenManagedStopFails(t *testing.T) {
	p, _ := startTestDaemon(t)
	home := t.TempDir()

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "linux"
	serviceUserHomeDir = func() (string, error) { return home, nil }

	unitPath := filepath.Join(home, ".config", "systemd", "user", systemdServiceName(p))
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, fmt.Errorf("user manager unavailable")
	}

	if err := Stop(p); err != nil {
		t.Fatalf("Stop should fall back to detached daemon shutdown: %v", err)
	}

	if len(commands) != 1 {
		t.Fatalf("expected one managed stop command before fallback, got %v", commands)
	}
	if want := "systemctl --user stop " + systemdServiceName(p); commands[0] != want {
		t.Fatalf("stop command = %q, want %q", commands[0], want)
	}

	alive, err := IsRunning(p)
	if err != nil {
		t.Fatal(err)
	}
	if alive {
		t.Fatal("daemon should be stopped")
	}
	if _, err := os.Stat(unitPath); err != nil {
		t.Fatalf("managed unit should remain installed after fallback stop: %v", err)
	}
	_ = os.Remove(unitPath)
	_ = os.Remove(filepath.Dir(unitPath))
	_ = os.Remove(filepath.Dir(filepath.Dir(unitPath)))
	_ = os.Remove(filepath.Dir(filepath.Dir(filepath.Dir(unitPath))))
}

func TestStopFallsBackToDetachedDaemonOnWindowsWithoutManagedService(t *testing.T) {
	p, _ := startTestDaemon(t)

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"
	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, fmt.Errorf("task not found")
	}

	if err := Stop(p); err != nil {
		t.Fatalf("Stop should fall back to detached daemon shutdown: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("expected one scheduled-task query, got %v", commands)
	}
	if want := "schtasks /Query /TN " + windowsTaskName(p); commands[0] != want {
		t.Fatalf("query command = %q, want %q", commands[0], want)
	}

	alive, err := IsRunning(p)
	if err != nil {
		t.Fatal(err)
	}
	if alive {
		t.Fatal("daemon should be stopped")
	}
}

// TestDefaultServiceManagerBypassedIsTrueUnderGoTest locks in the contract
// that protects developer machines: under `go test`, the default bypass
// function must short-circuit managed-service plumbing so unstubbed daemon
// tests cannot invoke real launchctl/systemctl/schtasks. A regression here
// previously caused TestStopNotRunningIsNoop to tear down the live
// LaunchAgent-managed daemon on macOS.
func TestDefaultServiceManagerBypassedIsTrueUnderGoTest(t *testing.T) {
	if !defaultServiceManagerBypassed() {
		t.Fatal("defaultServiceManagerBypassed() must return true under `go test` so daemon tests cannot reach real launchctl/systemctl/schtasks state")
	}
}

// TestStopWithUnstubbedPathsDoesNotInvokeRealServiceCommands is the
// end-to-end regression test: it simulates a managed service having been
// installed at the user's real-looking home (plist / systemd unit) and
// asserts that Stop(p), when called from a test binary with only a temp
// paths.Paths, does not invoke any service-manager commands. Before the
// fix, Stop went through stopManagedService -> managedServiceInstalled ->
// os.Stat(launchAgentPath()) which used the real os.UserHomeDir, found the
// real plist, then ran real `launchctl bootout` and killed the live daemon.
func TestStopWithUnstubbedPathsDoesNotInvokeRealServiceCommands(t *testing.T) {
	cleanup := stubServiceRuntime(t)
	defer cleanup()

	// Unlike other tests using stubServiceRuntime, we deliberately restore
	// the production bypass function. That is the code under test here.
	serviceManagerBypassed = defaultServiceManagerBypassed

	home := t.TempDir()
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceCurrentUser = func() (*user.User, error) { return &user.User{Uid: "99999"}, nil }
	runtimeGOOS = runtime.GOOS

	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	// Seed a managed-service artifact at the SCOPED path for this p so that
	// if the testing.Testing() bypass ever regresses, managedServiceInstalled(p)
	// would return true and Stop(p) would call serviceCommandRunner. We detect
	// that below.
	switch runtime.GOOS {
	case "darwin":
		plistDir := filepath.Join(home, "Library", "LaunchAgents")
		if err := os.MkdirAll(plistDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(plistDir, launchdServiceLabel(p)+".plist"), []byte("<plist/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	case "linux":
		unitDir := filepath.Join(home, ".config", "systemd", "user")
		if err := os.MkdirAll(unitDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(unitDir, systemdServiceName(p)), []byte("[Unit]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var called []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		called = append(called, name+" "+strings.Join(args, " "))
		// Pretend the task/service exists on Windows so the "is installed"
		// probe cannot short-circuit via an error return.
		return nil, nil
	}

	if err := Stop(p); err != nil {
		t.Fatalf("Stop(p) with unstubbed paths must be a no-op under go test, got error: %v", err)
	}
	if len(called) > 0 {
		t.Fatalf("Stop(p) under go test must not invoke any service-manager commands, got: %v", called)
	}
}

// TestStartWithUnstubbedPathsDoesNotInvokeRealServiceCommands mirrors the
// Stop regression for Start(). Start also goes through
// installManagedService -> startManagedService, both of which rely on the
// bypass guard to stay out of real launchctl/systemctl/schtasks when called
// from a test binary with only a temp paths.Paths.
func TestStartWithUnstubbedPathsDoesNotInvokeRealServiceCommands(t *testing.T) {
	cleanup := stubServiceRuntime(t)
	defer cleanup()

	serviceManagerBypassed = defaultServiceManagerBypassed

	// Force the detached fallback path to short-circuit as well: TestMain
	// already exits immediately when NM_DAEMON_HELPER_PROCESS=1, so the
	// re-exec does not spawn a persistent daemon.
	t.Setenv("NM_DAEMON_HELPER_PROCESS", "1")

	home := t.TempDir()
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceCurrentUser = func() (*user.User, error) { return &user.User{Uid: "99999"}, nil }
	serviceExecutablePath = func() (string, error) { return os.Args[0], nil }
	runtimeGOOS = runtime.GOOS

	var called []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		called = append(called, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	// Report "not running" for all health checks so Start exercises the
	// full managed-install-then-fallback decision tree.
	daemonHealthCheck = func(*paths.Paths) (bool, error) { return false, nil }

	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	// Start will fall back to the detached daemon which re-execs the test
	// binary; with NM_DAEMON_HELPER_PROCESS=1 the helper exits and the
	// health check never returns ok, so Start reports "did not become
	// responsive". That error is fine - we only care that no service
	// commands were invoked.
	_ = Start(p)
	if len(called) > 0 {
		t.Fatalf("Start(p) under go test must not invoke any service-manager commands, got: %v", called)
	}
}

func TestServiceInstanceSuffixResolvesSymlinkedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup is environment-dependent on Windows")
	}

	base := t.TempDir()
	realRoot := filepath.Join(base, "real", "nm-home")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(base, "alias")
	if err := os.Symlink(filepath.Join(base, "real"), linkRoot); err != nil {
		t.Skipf("symlink setup unavailable: %v", err)
	}

	realPaths := paths.WithRoot(realRoot)
	aliasPaths := paths.WithRoot(filepath.Join(linkRoot, "nm-home"))

	if got, want := serviceInstanceSuffix(aliasPaths), serviceInstanceSuffix(realPaths); got != want {
		t.Fatalf("serviceInstanceSuffix(alias) = %q, want %q", got, want)
	}
	if got, want := launchdServiceLabel(aliasPaths), launchdServiceLabel(realPaths); got != want {
		t.Fatalf("launchdServiceLabel(alias) = %q, want %q", got, want)
	}
	if got, want := systemdServiceName(aliasPaths), systemdServiceName(realPaths); got != want {
		t.Fatalf("systemdServiceName(alias) = %q, want %q", got, want)
	}
	if got, want := windowsTaskName(aliasPaths), windowsTaskName(realPaths); got != want {
		t.Fatalf("windowsTaskName(alias) = %q, want %q", got, want)
	}
}

func TestServiceInstanceSuffixKeepsRelativeRootStableAcrossWorkingDirs(t *testing.T) {
	cleanup := stubServiceRuntime(t)
	defer cleanup()

	base := t.TempDir()
	firstWD := filepath.Join(base, "first")
	secondWD := filepath.Join(base, "second")
	for _, dir := range []string{firstWD, secondWD} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(originalWD); err != nil {
			t.Fatalf("restore working directory: %v", err)
		}
	}()

	relativePaths := paths.WithRoot(filepath.Join(".", "nm-home"))

	if err := os.Chdir(firstWD); err != nil {
		t.Fatal(err)
	}
	first := serviceInstanceSuffix(relativePaths)

	if err := os.Chdir(secondWD); err != nil {
		t.Fatal(err)
	}
	second := serviceInstanceSuffix(relativePaths)

	if first != second {
		t.Fatalf("serviceInstanceSuffix(relative root) changed across working directories: %q vs %q", first, second)
	}
}

func TestServiceInstanceSuffixNormalizesCaseOnWindows_EnableWindowsCI(t *testing.T) {
	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"

	base := filepath.Join(t.TempDir(), "Nm-Home")
	upper := paths.WithRoot(strings.ToUpper(base))
	lower := paths.WithRoot(strings.ToLower(base))

	if got, want := serviceInstanceSuffix(upper), serviceInstanceSuffix(lower); got != want {
		t.Fatalf("serviceInstanceSuffix(upper) = %q, want %q", got, want)
	}
}

// TestStopDoesNotTouchManagedDaemonOwnedByDifferentNMHome is the structural
// regression test for the per-NM_HOME scoping. Before scoping, the launchd
// label / systemd unit / Windows task name were globally unique per user.
// Any `go test ./internal/daemon` in any checkout - including worktrees
// without the testing.Testing() bypass - called TestStopNotRunningIsNoop
// -> Stop(tmpdir-p), which matched the global identifier and tore down the
// user's live LaunchAgent-managed daemon. This test pins in the scoping:
// with serviceManagerBypassed explicitly disabled (simulating worktrees
// without the testing.Testing() guard), Stop(p) for a tmpdir paths.Paths
// must still not invoke any destructive service-manager command against
// artifacts owned by a different NM_HOME.
func TestStopDoesNotTouchManagedDaemonOwnedByDifferentNMHome(t *testing.T) {
	cleanup := stubServiceRuntime(t)
	defer cleanup()

	// Explicitly bypass the testing.Testing() guard. This simulates a
	// test binary compiled from a codebase that predates or lacks the
	// bypass, which is exactly the failure mode observed in pipeline
	// worktrees rebased onto older main branches.
	serviceManagerBypassed = func() bool { return false }

	home := t.TempDir()
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceCurrentUser = func() (*user.User, error) { return &user.User{Uid: "99999"}, nil }
	runtimeGOOS = runtime.GOOS

	// Simulate a live managed daemon owned by a DIFFERENT NM_HOME - i.e.
	// the user's real ~/.no-mistakes - by seeding the artifact that an
	// older unscoped binary would have installed (the legacy global name),
	// plus the scoped artifact a modern binary would install for that
	// other NM_HOME. Stop(p) for a test p.Root() must touch neither.
	otherP := paths.WithRoot(filepath.Join(home, "real-nm-home"))
	switch runtime.GOOS {
	case "darwin":
		plistDir := filepath.Join(home, "Library", "LaunchAgents")
		if err := os.MkdirAll(plistDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(plistDir, legacyLaunchdServiceLabel+".plist"), []byte("<plist/>"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(plistDir, launchdServiceLabel(otherP)+".plist"), []byte("<plist/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	case "linux":
		unitDir := filepath.Join(home, ".config", "systemd", "user")
		if err := os.MkdirAll(unitDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(unitDir, legacySystemdServiceName), []byte("[Unit]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(unitDir, systemdServiceName(otherP)), []byte("[Unit]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var called []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		called = append(called, name+" "+strings.Join(args, " "))
		// For Windows: pretend the scoped task for this test p is NOT
		// installed (the test p has its own scoped suffix, different from
		// the "owner" otherP). Any query for the legacy or otherP's task
		// would still succeed, but only the test-p query path reaches
		// serviceCommandRunner inside managedServiceInstalled(p).
		return nil, fmt.Errorf("not found")
	}

	p := paths.WithRoot(filepath.Join(t.TempDir(), "test-nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	if err := Stop(p); err != nil {
		t.Fatalf("Stop(p) should be a no-op when no managed daemon is owned by this NM_HOME: %v", err)
	}
	for _, cmd := range called {
		// Destructive subcommands that would tear down another NM_HOME's daemon.
		for _, forbidden := range []string{"bootout", "/End", "/Delete", "--user stop", "--user disable"} {
			if strings.Contains(cmd, forbidden) {
				t.Fatalf("Stop(p) must not touch managed daemon owned by a different NM_HOME, got destructive command: %q", cmd)
			}
		}
	}
}

func stubServiceRuntime(t *testing.T) func() {
	t.Helper()
	oldGOOS := runtimeGOOS
	oldUserHomeDir := serviceUserHomeDir
	oldCurrentUser := serviceCurrentUser
	oldExecutablePath := serviceExecutablePath
	oldCommandRunner := serviceCommandRunner
	oldHealthCheck := daemonHealthCheck
	oldServiceBypass := serviceManagerBypassed
	serviceManagerBypassed = func() bool { return false }
	return func() {
		runtimeGOOS = oldGOOS
		serviceUserHomeDir = oldUserHomeDir
		serviceCurrentUser = oldCurrentUser
		serviceExecutablePath = oldExecutablePath
		serviceCommandRunner = oldCommandRunner
		daemonHealthCheck = oldHealthCheck
		serviceManagerBypassed = oldServiceBypass
	}
}
