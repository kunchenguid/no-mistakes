package daemon

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
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

	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchdServiceLabel+".plist")
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
	if want := "launchctl bootout gui/501/" + launchdServiceLabel; commands[0] != want {
		t.Fatalf("bootout command = %q, want %q", commands[0], want)
	}
	if want := "launchctl bootstrap gui/501 " + plistPath; commands[1] != want {
		t.Fatalf("bootstrap command = %q, want %q", commands[1], want)
	}
	if want := "launchctl kickstart -k gui/501/" + launchdServiceLabel; commands[2] != want {
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

	unitPath := filepath.Join(home, ".config", "systemd", "user", systemdServiceName)
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
		"systemctl --user enable " + systemdServiceName,
		"systemctl --user start " + systemdServiceName,
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
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
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

	wantCreate := "schtasks /Create /TN " + windowsTaskName +
		" /SC ONLOGON /RL LIMITED /F /TR " + buildWindowsTaskCommand(exe, p.Root())
	wantRun := "schtasks /Run /TN " + windowsTaskName
	if len(commands) != 2 {
		t.Fatalf("expected schtasks create and run, got %v", commands)
	}
	if commands[0] != wantCreate {
		t.Fatalf("create command = %q, want %q", commands[0], wantCreate)
	}
	if commands[1] != wantRun {
		t.Fatalf("run command = %q, want %q", commands[1], wantRun)
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
		if command == "systemctl --user stop "+systemdServiceName {
			managedStopped = true
			return nil, nil
		}
		if command == "systemctl --user start "+systemdServiceName {
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
		"systemctl --user enable " + systemdServiceName,
		"systemctl --user start " + systemdServiceName,
		"systemctl --user stop " + systemdServiceName,
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
	if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user", systemdServiceName)); err != nil {
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
		if command == "systemctl --user stop "+systemdServiceName {
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
		"systemctl --user enable " + systemdServiceName,
		"systemctl --user start " + systemdServiceName,
		"systemctl --user stop " + systemdServiceName,
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

	unitPath := filepath.Join(home, ".config", "systemd", "user", systemdServiceName)
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
	if want := "systemctl --user stop " + systemdServiceName; commands[0] != want {
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

	unitPath := filepath.Join(home, ".config", "systemd", "user", systemdServiceName)
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
	if want := "systemctl --user stop " + systemdServiceName; commands[0] != want {
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
	if want := "schtasks /Query /TN " + windowsTaskName; commands[0] != want {
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
