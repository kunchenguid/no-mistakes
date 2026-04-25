package daemon

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestStopLaunchAgentTreatsBootoutNotLoadedAsSuccess locks in that
// `launchctl bootout` returning ESRCH ("No such process", exit 3) is
// treated as a successful no-op. launchctl emits this when the service
// label isn't currently loaded, which is semantically the same as "already
// stopped" for stop purposes. Without this, a plain `daemon stop` after
// the plist was unloaded out-of-band surfaces a scary error and the
// detached-fallback path in Start() short-circuits on an install where
// bootstrap/kickstart failed and the service was never loaded.
func TestStopLaunchAgentTreatsBootoutNotLoadedAsSuccess(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	home := t.TempDir()

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "darwin"
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceCurrentUser = func() (*user.User, error) { return &user.User{Uid: "501"}, nil }

	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		target := "gui/501/" + launchdServiceLabel(p)
		return []byte("Boot-out failed: 3: No such process"),
			fmt.Errorf("/bin/launchctl bootout %s: exit status 3: Boot-out failed: 3: No such process", target)
	}

	if err := stopLaunchAgent(p); err != nil {
		t.Fatalf("stopLaunchAgent should treat ESRCH bootout as success, got: %v", err)
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

// TestStartLaunchAgentRetriesBootstrapOnEPROGRESS locks in the fix for the
// stop+start race observed during `make install`: launchctl bootout is
// async and SIGTERMs the old service while keeping the label registered
// for up to ~5s. A bootstrap in that window returns exit 37 "Operation
// already in progress". Without retry, the caller sees a hard failure and
// falls back to the detached-daemon path, dropping the plist on the floor -
// which breaks auto-start on reboot and was the reason the #143 PATH fix
// never actually reached the user's launchd daemon.
func TestStartLaunchAgentRetriesBootstrapOnEPROGRESS(t *testing.T) {
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

	oldInterval := launchctlBootstrapRetryInterval
	oldTimeout := launchctlBootstrapRetryTimeout
	launchctlBootstrapRetryInterval = time.Millisecond
	launchctlBootstrapRetryTimeout = 200 * time.Millisecond
	defer func() {
		launchctlBootstrapRetryInterval = oldInterval
		launchctlBootstrapRetryTimeout = oldTimeout
	}()

	var bootstrapAttempts int
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		if name == "launchctl" && len(args) > 0 && args[0] == "bootstrap" {
			bootstrapAttempts++
			if bootstrapAttempts < 3 {
				return []byte("Bootstrap failed: 37: Operation already in progress"),
					fmt.Errorf("/bin/launchctl bootstrap: exit status 37: Bootstrap failed: 37: Operation already in progress")
			}
		}
		return nil, nil
	}

	if err := startLaunchAgent(p); err != nil {
		t.Fatalf("startLaunchAgent should retry bootstrap on EPROGRESS, got %v", err)
	}
	if bootstrapAttempts < 3 {
		t.Fatalf("expected bootstrap to retry until success, got %d attempts", bootstrapAttempts)
	}
}

// TestStartLaunchAgentDoesNotRetryNonBusyBootstrapErrors ensures that
// genuine failures (bad plist, missing binary, permissions) surface fast
// rather than being silently retried for 10 seconds.
func TestStartLaunchAgentDoesNotRetryNonBusyBootstrapErrors(t *testing.T) {
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

	oldInterval := launchctlBootstrapRetryInterval
	oldTimeout := launchctlBootstrapRetryTimeout
	launchctlBootstrapRetryInterval = time.Millisecond
	launchctlBootstrapRetryTimeout = 200 * time.Millisecond
	defer func() {
		launchctlBootstrapRetryInterval = oldInterval
		launchctlBootstrapRetryTimeout = oldTimeout
	}()

	var bootstrapAttempts int
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		if name == "launchctl" && len(args) > 0 && args[0] == "bootstrap" {
			bootstrapAttempts++
			return []byte("Bootstrap failed: Path had bad ownership/permissions"),
				fmt.Errorf("launchctl bootstrap: exit status 78: permissions")
		}
		// kickstart fails when bootstrap never completed; that's fine for
		// this test since we only care bootstrap didn't retry.
		return nil, fmt.Errorf("no service")
	}

	_ = startLaunchAgent(p)
	if bootstrapAttempts != 1 {
		t.Fatalf("expected bootstrap to run once for non-busy errors, got %d attempts", bootstrapAttempts)
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
