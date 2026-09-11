package shellenv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// missingShellError is what os/exec returns when the login shell binary does
// not exist: fork/exec on an absolute path fails with ENOENT. On macOS with
// nix-darwin this is the state of /run/current-system/sw/bin/zsh between boot
// and the activate-system daemon recreating the symlink (#143).
func missingShellError(shell string) error {
	return &fs.PathError{Op: "fork/exec", Path: shell, Err: syscall.ENOENT}
}

type retryProbe struct {
	t        *testing.T
	failures int
	calls    int
	sleeps   []time.Duration
}

func (p *retryProbe) install(t *testing.T) {
	t.Helper()
	oldOutput := shellCommandOutput
	oldSleep := shellRetrySleep
	shellCommandOutput = func(shell string, _ ...string) ([]byte, error) {
		p.calls++
		if p.calls <= p.failures {
			return nil, missingShellError(shell)
		}
		return []byte("PATH=/nix/profile/bin:/usr/bin\x00HOME=/Users/test\x00"), nil
	}
	shellRetrySleep = func(d time.Duration) { p.sleeps = append(p.sleeps, d) }
	t.Cleanup(func() {
		shellCommandOutput = oldOutput
		shellRetrySleep = oldSleep
		resetForTests()
	})
}

func TestResolveWithShellRetry_WaitsForAMissingLoginShellBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Resolve short-circuits to os.Environ() on Windows")
	}
	resetForTests()
	t.Setenv("SHELL", "/run/current-system/sw/bin/zsh")
	t.Setenv("HOME", "/Users/test")
	t.Setenv("PATH", "/usr/bin:/bin")
	probe := &retryProbe{t: t, failures: 2}
	probe.install(t)

	env, err := ResolveWithShellRetry(DefaultShellRetryWindow)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := envValue(env, "PATH")
	if !strings.HasPrefix(path, "/nix/profile/bin:") {
		t.Fatalf("expected the login shell PATH once the shell appeared, got %q", path)
	}
	if probe.calls != 3 {
		t.Fatalf("probe calls = %d, want 3 (two misses, then success)", probe.calls)
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; fmt.Sprint(probe.sleeps) != fmt.Sprint(want) {
		t.Fatalf("retry sleeps = %v, want %v", probe.sleeps, want)
	}
	if Degraded() {
		t.Fatal("a successful retry must clear the degraded state")
	}
	// The recovered result is the cached one: later calls must not re-probe.
	shellCommandOutput = func(string, ...string) ([]byte, error) {
		t.Fatal("unexpected re-probe after a successful retry")
		return nil, nil
	}
	if cached, err := Resolve(); err != nil || fmt.Sprint(cached) != fmt.Sprint(env) {
		t.Fatalf("cached resolution = %v, %v; want %v", cached, err, env)
	}
}

func TestResolveWithShellRetry_BoundsWaitingForAPermanentlyMissingShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Resolve short-circuits to os.Environ() on Windows")
	}
	resetForTests()
	t.Setenv("SHELL", "/run/current-system/sw/bin/zsh")
	t.Setenv("HOME", "/Users/test")
	t.Setenv("PATH", "/usr/bin:/bin")
	probe := &retryProbe{t: t, failures: 1 << 20}
	probe.install(t)
	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	env, err := ResolveWithShellRetry(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Backoff 1s + 2s fits the 5s window; the next 4s does not, so waiting
	// stops there and the degraded fallback is used.
	if want := []time.Duration{time.Second, 2 * time.Second}; fmt.Sprint(probe.sleeps) != fmt.Sprint(want) {
		t.Fatalf("retry sleeps = %v, want %v", probe.sleeps, want)
	}
	if probe.calls != 3 {
		t.Fatalf("probe calls = %d, want 3", probe.calls)
	}
	if !Degraded() {
		t.Fatal("expected the degraded state after the window is exhausted")
	}
	path, _ := envValue(env, "PATH")
	if !strings.HasPrefix(path, "/usr/bin:/bin") || !strings.Contains(path, "/opt/homebrew/bin") {
		t.Fatalf("expected the augmented process-environment fallback, got %q", path)
	}
	if !strings.Contains(buf.String(), "login shell binary is missing") || !strings.Contains(buf.String(), "login shell environment resolution failed") {
		t.Fatalf("expected retry and fallback warnings, got %q", buf.String())
	}
}

func TestResolveWithShellRetry_DoesNotWaitForAShellThatExistsButFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Resolve short-circuits to os.Environ() on Windows")
	}
	resetForTests()
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("HOME", "/Users/test")
	for name, probeErr := range map[string]error{
		"non-zero exit": &exec.ExitError{},
		"timeout":       context.DeadlineExceeded,
	} {
		t.Run(name, func(t *testing.T) {
			resetForTests()
			probe := &retryProbe{t: t}
			probe.install(t)
			shellCommandOutput = func(string, ...string) ([]byte, error) {
				probe.calls++
				return nil, probeErr
			}
			if _, err := ResolveWithShellRetry(DefaultShellRetryWindow); err != nil {
				t.Fatal(err)
			}
			if len(probe.sleeps) != 0 || probe.calls != 1 {
				t.Fatalf("sleeps = %v, calls = %d; a failing shell that exists must not be waited for", probe.sleeps, probe.calls)
			}
			if !Degraded() {
				t.Fatal("expected the degraded state")
			}
		})
	}
}

func TestResolve_ReportsDegradedUntilAProbeSucceeds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Resolve short-circuits to os.Environ() on Windows")
	}
	resetForTests()
	t.Setenv("SHELL", "/run/current-system/sw/bin/zsh")
	t.Setenv("HOME", "/Users/test")
	probe := &retryProbe{t: t, failures: 1}
	probe.install(t)

	if Degraded() {
		t.Fatal("nothing resolved yet, so nothing is degraded")
	}
	if _, err := Resolve(); err != nil {
		t.Fatal(err)
	}
	if !Degraded() {
		t.Fatal("expected Degraded after a fallback resolution")
	}
	env, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if Degraded() {
		t.Fatal("expected a later successful probe to clear Degraded")
	}
	if path, _ := envValue(env, "PATH"); !strings.HasPrefix(path, "/nix/profile/bin:") {
		t.Fatalf("expected the recovered login shell PATH, got %q", path)
	}
}

// TestDefaultShellCommandOutput_InteractiveShellFromForegroundTerminal pins
// the probe against the way a terminal user runs `daemon run`: as the
// foreground process of a pty. An interactive zsh spawned in its own process
// group from there stops itself with SIGTTIN and the probe only ends when the
// timeout kills it, so the daemon degrades. The probe must complete in its
// own session instead. `script` allocates a pty and makes the re-executed
// test binary its session leader and foreground group; the child half of the
// test runs the real probe there and prints a marker on success.
func TestDefaultShellCommandOutput_InteractiveShellFromForegroundTerminal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX sessions or controlling terminals")
	}
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is not installed")
	}
	const marker = "SHELLENV_PTY_PROBE_OK"
	if os.Getenv("NM_SHELLENV_PTY_CHILD") == "1" {
		shellCommandTimeout = 10 * time.Second
		out, err := defaultShellCommandOutput(zsh, "-l", "-i", "-c", "env -0")
		if err != nil {
			fmt.Printf("probe failed: %v\n", err)
			return
		}
		if _, ok := envValue(parseEnvOutput(out), "PATH"); !ok {
			fmt.Printf("probe printed no PATH: %q\n", out)
			return
		}
		fmt.Println(marker)
		return
	}
	scriptBin, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script(1) is not installed")
	}
	child := os.Args[0] + " -test.run ^TestDefaultShellCommandOutput_InteractiveShellFromForegroundTerminal$ -test.count=1"
	var args []string
	if runtime.GOOS == "darwin" {
		args = append([]string{"-q", "/dev/null"}, strings.Fields(child)...)
	} else {
		args = []string{"-q", "-c", child, "/dev/null"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, scriptBin, args...)
	cmd.Env = append(os.Environ(), "NM_SHELLENV_PTY_CHILD=1", "SHELL="+zsh)
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("pty child did not finish: %q", out)
	}
	if !strings.Contains(string(out), marker) {
		t.Fatalf("interactive login shell probe did not succeed from a foreground terminal (err=%v):\n%s", err, out)
	}
}
