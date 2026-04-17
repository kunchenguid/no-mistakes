package daemon

import (
	"os"
	"os/user"
	"path/filepath"
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

func TestInstallLaunchAgentDoesNotRemoveLegacyPlistForDifferentRoot(t *testing.T) {
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

	legacyPath := filepath.Join(home, "Library", "LaunchAgents", legacyLaunchdServiceLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	otherRoot := filepath.Join(t.TempDir(), "other-nm-home")
	legacyPlist := renderLaunchAgent("/opt/no-mistakes/bin/no-mistakes", paths.WithRoot(otherRoot), home)
	if err := os.WriteFile(legacyPath, []byte(legacyPlist), 0o644); err != nil {
		t.Fatal(err)
	}

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, nil
	}

	if err := installLaunchAgent(p, "/opt/no-mistakes/bin/no-mistakes"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy plist for different root should remain: %v", err)
	}
	if len(commands) != 0 {
		t.Fatalf("install should not boot out unrelated legacy daemon, got commands %v", commands)
	}
}
