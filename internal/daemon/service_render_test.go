package daemon

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestServiceDefinitionMatchesRootRejectsPrefixOnlyMatch(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "nm")
	otherRoot := root + "-prod"
	home := t.TempDir()
	p := paths.WithRoot(root)
	other := paths.WithRoot(otherRoot)

	if serviceDefinitionMatchesRoot([]byte(renderLaunchAgent("/opt/no-mistakes/bin/no-mistakes", other, home)), p) {
		t.Fatal("expected launch agent root prefix collision to be rejected")
	}
	if serviceDefinitionMatchesRoot([]byte(renderSystemdUnit("/usr/local/bin/no-mistakes", other, home)), p) {
		t.Fatal("expected systemd root prefix collision to be rejected")
	}
	if serviceDefinitionMatchesRoot([]byte(`<Task><Exec><Command>C:\nm.exe</Command><Arguments>`+buildWindowsTaskCommand(`C:\nm.exe`, otherRoot)+`</Arguments></Exec></Task>`), p) {
		t.Fatal("expected windows task root prefix collision to be rejected")
	}

	if !serviceDefinitionMatchesRoot([]byte(renderLaunchAgent("/opt/no-mistakes/bin/no-mistakes", p, home)), p) {
		t.Fatal("expected exact launch agent root match")
	}
	if !serviceDefinitionMatchesRoot([]byte(renderSystemdUnit("/usr/local/bin/no-mistakes", p, home)), p) {
		t.Fatal("expected exact systemd root match")
	}
	if !serviceDefinitionMatchesRoot([]byte(`<Task><Exec><Command>C:\nm.exe</Command><Arguments>`+buildWindowsTaskCommand(`C:\nm.exe`, root)+`</Arguments></Exec></Task>`), p) {
		t.Fatal("expected exact windows task root match")
	}
}

// TestRenderLaunchAgentIncludesManagedPath locks in that the generated plist
// ships a sensible PATH in EnvironmentVariables. Without this, launchd runs
// the daemon with `/usr/bin:/bin:/usr/sbin:/sbin` and the agent binary (at
// e.g. /opt/homebrew/bin/codex) is invisible to exec.LookPath. See #143.
func TestRenderLaunchAgentIncludesManagedPath(t *testing.T) {
	t.Parallel()
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm"))
	home := "/Users/test"

	plist := renderLaunchAgent("/opt/no-mistakes/bin/no-mistakes", p, home)

	if !strings.Contains(plist, "<key>PATH</key>") {
		t.Fatalf("expected PATH entry in launchd plist, got:\n%s", plist)
	}
	pathValue := extractPlistValue(t, plist, "PATH")
	for _, want := range []string{
		"/opt/homebrew/bin",
		"/opt/homebrew/sbin",
		"/usr/local/bin",
		"/usr/local/sbin",
		"/usr/bin",
		"/bin",
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "go", "bin"),
		filepath.Join(home, ".cargo", "bin"),
	} {
		if !strings.Contains(pathValue, want) {
			t.Fatalf("expected plist PATH to contain %q, got %q", want, pathValue)
		}
	}
}

// TestRenderSystemdUnitIncludesManagedPath mirrors the launchd coverage for
// systemd user services so Linux installs don't regress into the same
// "agent binary not in PATH" failure mode when launched at login.
func TestRenderSystemdUnitIncludesManagedPath(t *testing.T) {
	t.Parallel()
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm"))
	home := "/home/test"

	unit := renderSystemdUnit("/usr/local/bin/no-mistakes", p, home)
	for _, want := range []string{
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "go", "bin"),
		filepath.Join(home, ".cargo", "bin"),
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("expected systemd unit to contain PATH entry %q, got:\n%s", want, unit)
		}
	}
	if !strings.Contains(unit, "PATH=") {
		t.Fatalf("expected Environment=PATH=... in systemd unit, got:\n%s", unit)
	}
}

// extractPlistValue pulls the <string> value that follows a given <key> in
// an Apple plist. Keeps the rendering assertions readable and independent
// of byte-for-byte formatting.
func extractPlistValue(t *testing.T, plist, key string) string {
	t.Helper()
	keyTag := "<key>" + key + "</key>"
	idx := strings.Index(plist, keyTag)
	if idx < 0 {
		t.Fatalf("key %q not found in plist", key)
	}
	rest := plist[idx+len(keyTag):]
	start := strings.Index(rest, "<string>")
	end := strings.Index(rest, "</string>")
	if start < 0 || end < 0 || end < start {
		t.Fatalf("malformed plist string for key %q: %q", key, rest)
	}
	return rest[start+len("<string>") : end]
}
