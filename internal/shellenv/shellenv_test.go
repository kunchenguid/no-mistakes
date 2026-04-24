package shellenv

import (
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestResolve_UsesLoginShellAndCapturesEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Resolve short-circuits to os.Environ() on Windows")
	}
	resetForTests()
	t.Setenv("SHELL", "/bin/bash")

	oldOutput := shellCommandOutput
	defer func() {
		shellCommandOutput = oldOutput
		resetForTests()
	}()

	var gotShell string
	var gotArgs []string
	shellCommandOutput = func(shell string, args ...string) ([]byte, error) {
		gotShell = shell
		gotArgs = append([]string(nil), args...)
		return []byte("PATH=/resolved/bin\x00HOME=/Users/test\x00SPECIAL=1\x00"), nil
	}

	env, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if gotShell != "/bin/bash" {
		t.Fatalf("shell = %q, want %q", gotShell, "/bin/bash")
	}
	if !reflect.DeepEqual(gotArgs, []string{"-l", "-i", "-c", "env -0"}) {
		t.Fatalf("shell args = %v", gotArgs)
	}
	for _, want := range []string{"HOME=/Users/test", "SPECIAL=1"} {
		if !containsEnvEntry(env, want) {
			t.Fatalf("expected resolved env to contain %q, got %v", want, env)
		}
	}
	path, ok := envValue(env, "PATH")
	if !ok {
		t.Fatalf("expected PATH in resolved env, got %v", env)
	}
	if !strings.HasPrefix(path, "/resolved/bin") {
		t.Fatalf("expected shell-provided PATH entries first, got %q", path)
	}
}

func TestApplyToProcess_SetsResolvedEnvEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Resolve short-circuits to os.Environ() on Windows")
	}
	resetForTests()
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("KEEP_ME", "1")

	oldOutput := shellCommandOutput
	defer func() {
		shellCommandOutput = oldOutput
		resetForTests()
	}()

	shellCommandOutput = func(shell string, args ...string) ([]byte, error) {
		return []byte("PATH=/resolved/bin\x00HOME=/Users/test\x00SPECIAL=1\x00"), nil
	}

	if err := ApplyToProcess(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("PATH"); !strings.HasPrefix(got, "/resolved/bin") {
		t.Fatalf("PATH = %q, expected shell-provided entries first", got)
	}
	if got := os.Getenv("HOME"); got != "/Users/test" {
		t.Fatalf("HOME = %q", got)
	}
	if got := os.Getenv("SPECIAL"); got != "1" {
		t.Fatalf("SPECIAL = %q", got)
	}
	if got := os.Getenv("KEEP_ME"); got != "1" {
		t.Fatalf("KEEP_ME = %q", got)
	}
}

func TestResolve_ReturnsProcessEnvOnWindows(t *testing.T) {
	resetForTests()
	oldGOOS := runtimeGOOS
	oldOutput := shellCommandOutput
	defer func() {
		runtimeGOOS = oldGOOS
		shellCommandOutput = oldOutput
		resetForTests()
	}()

	runtimeGOOS = "windows"
	t.Setenv("SHELLENV_WINDOWS_RESOLVE", "1")
	shellCommandOutput = func(string, ...string) ([]byte, error) {
		t.Fatal("Resolve should not shell out on Windows")
		return nil, nil
	}

	env, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if !containsEnvEntry(env, "SHELLENV_WINDOWS_RESOLVE=1") {
		t.Fatalf("expected resolved env to contain process env entry, got %v", env)
	}
}

func TestApplyToProcess_UsesProcessEnvOnWindows(t *testing.T) {
	resetForTests()
	oldGOOS := runtimeGOOS
	oldOutput := shellCommandOutput
	defer func() {
		runtimeGOOS = oldGOOS
		shellCommandOutput = oldOutput
		resetForTests()
	}()

	runtimeGOOS = "windows"
	t.Setenv("SHELLENV_WINDOWS_APPLY", "1")
	shellCommandOutput = func(string, ...string) ([]byte, error) {
		t.Fatal("ApplyToProcess should not shell out on Windows")
		return nil, nil
	}

	if err := ApplyToProcess(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("SHELLENV_WINDOWS_APPLY"); got != "1" {
		t.Fatalf("SHELLENV_WINDOWS_APPLY = %q", got)
	}
}

func TestParseEnvOutput_IgnoresShellNoiseBeforeEnv(t *testing.T) {
	env := parseEnvOutput([]byte("banner text\nPATH=/resolved/bin\x00HOME=/Users/test\x00SPECIAL=1\x00"))

	want := []string{"PATH=/resolved/bin", "HOME=/Users/test", "SPECIAL=1"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("env = %v, want %v", env, want)
	}
}

func TestDefaultShellCommandOutput_TimesOut(t *testing.T) {
	oldTimeout := shellCommandTimeout
	defer func() {
		shellCommandTimeout = oldTimeout
	}()

	shellCommandTimeout = 20 * time.Millisecond
	start := time.Now()
	_, err := defaultShellCommandOutput("/bin/sh", "-c", "sleep 1")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("command ran too long: %v", elapsed)
	}
}

func containsEnvEntry(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

// TestResolve_AugmentsPathWithWellKnownDirs covers the core launchd/systemd
// first-run issue: the managed daemon starts with a minimal PATH from the
// service manager, the login-shell probe fails or silently returns a bare
// PATH, and exec.LookPath for the agent binary fails. Well-known install
// dirs must be merged in so Homebrew-installed agents are still discoverable.
func TestResolve_AugmentsPathWithWellKnownDirs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Resolve short-circuits to os.Environ() on Windows")
	}
	resetForTests()
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("HOME", "/Users/test")

	oldOutput := shellCommandOutput
	defer func() {
		shellCommandOutput = oldOutput
		resetForTests()
	}()
	shellCommandOutput = func(shell string, args ...string) ([]byte, error) {
		return []byte("PATH=/usr/bin:/bin\x00HOME=/Users/test\x00"), nil
	}

	env, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	path, ok := envValue(env, "PATH")
	if !ok {
		t.Fatalf("expected PATH in resolved env, got %v", env)
	}
	for _, want := range []string{
		"/opt/homebrew/bin",
		"/opt/homebrew/sbin",
		"/usr/local/bin",
		"/usr/local/sbin",
		"/Users/test/.local/bin",
		"/Users/test/go/bin",
		"/Users/test/.cargo/bin",
	} {
		if !strings.Contains(path, want) {
			t.Fatalf("expected PATH to contain %q, got %q", want, path)
		}
	}
	// Caller-provided PATH entries must come first so they take precedence.
	if !strings.HasPrefix(path, "/usr/bin:/bin:") {
		t.Fatalf("expected original PATH entries at start, got %q", path)
	}
}

// TestResolve_DoesNotDuplicatePathEntries protects against PATH bloat when
// the login shell already exposes some of the well-known dirs. Duplicates
// don't break exec.LookPath, but they would grow PATH unboundedly across
// restarts if we appended blindly.
func TestResolve_DoesNotDuplicatePathEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Resolve short-circuits to os.Environ() on Windows")
	}
	resetForTests()
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("HOME", "/Users/test")

	oldOutput := shellCommandOutput
	defer func() {
		shellCommandOutput = oldOutput
		resetForTests()
	}()
	shellCommandOutput = func(string, ...string) ([]byte, error) {
		return []byte("PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin\x00"), nil
	}

	env, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	path, _ := envValue(env, "PATH")
	counts := map[string]int{}
	for _, entry := range strings.Split(path, string(os.PathListSeparator)) {
		counts[entry]++
	}
	for _, entry := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin"} {
		if counts[entry] != 1 {
			t.Fatalf("entry %q should appear exactly once, got %d in %q", entry, counts[entry], path)
		}
	}
}

// TestResolve_SynthesizesPathWhenMissing covers the failure mode where the
// login shell returns an env with no PATH at all (e.g., when $SHELL is a
// non-standard shell that doesn't source profile files). Without this, the
// daemon would end up with an empty PATH and nothing findable.
func TestResolve_SynthesizesPathWhenMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Resolve short-circuits to os.Environ() on Windows")
	}
	resetForTests()
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("HOME", "/Users/test")

	oldOutput := shellCommandOutput
	defer func() {
		shellCommandOutput = oldOutput
		resetForTests()
	}()
	shellCommandOutput = func(string, ...string) ([]byte, error) {
		return []byte("HOME=/Users/test\x00OTHER=1\x00"), nil
	}

	env, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	path, ok := envValue(env, "PATH")
	if !ok || path == "" {
		t.Fatalf("expected PATH to be synthesized from well-known dirs, got env=%v", env)
	}
	if !strings.Contains(path, "/opt/homebrew/bin") {
		t.Fatalf("expected /opt/homebrew/bin in synthesized PATH, got %q", path)
	}
	for _, want := range []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		if !strings.Contains(path, want) {
			t.Fatalf("expected synthesized PATH to contain %q, got %q", want, path)
		}
	}
}

// TestResolve_FallbackOnShellFailure covers the case where the login shell
// invocation itself fails (timeout, binary missing, non-zero exit). The
// existing fallback used os.Environ() as-is; now it should also merge in
// well-known dirs so launchd's minimal PATH is still usable.
func TestResolve_FallbackOnShellFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Resolve short-circuits to os.Environ() on Windows")
	}
	resetForTests()
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("HOME", "/Users/test")
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")

	oldOutput := shellCommandOutput
	defer func() {
		shellCommandOutput = oldOutput
		resetForTests()
	}()
	shellCommandOutput = func(string, ...string) ([]byte, error) {
		return nil, &noSuchFileError{}
	}

	env, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	path, _ := envValue(env, "PATH")
	if !strings.Contains(path, "/opt/homebrew/bin") {
		t.Fatalf("expected fallback PATH to include well-known dirs, got %q", path)
	}
}

type noSuchFileError struct{}

func (noSuchFileError) Error() string { return "no such file or directory" }
