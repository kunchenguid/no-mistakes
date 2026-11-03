package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
)

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "nm-main-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create test NM_HOME: %v\n", err)
		os.Exit(1)
	}
	home, err := os.MkdirTemp("", "nm-main-home-")
	if err != nil {
		_ = os.RemoveAll(root)
		fmt.Fprintf(os.Stderr, "create test HOME: %v\n", err)
		os.Exit(1)
	}
	_ = os.Setenv("NM_HOME", root)
	_ = os.Setenv("HOME", home)
	_ = os.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	_ = os.Setenv("NO_MISTAKES_TELEMETRY", "off")
	_ = os.Setenv("NO_MISTAKES_NO_UPDATE_CHECK", "1")

	code := m.Run()

	_ = os.RemoveAll(root)
	_ = os.RemoveAll(home)
	os.Exit(code)
}

func TestRunAttemptsOldExecutableCleanupBeforeEarlyRoutes(t *testing.T) {
	originalArgs := os.Args
	originalCleanup := cleanupOldExecutable
	originalBackground := maybeHandleBackgroundCheck
	t.Cleanup(func() {
		os.Args = originalArgs
		cleanupOldExecutable = originalCleanup
		maybeHandleBackgroundCheck = originalBackground
	})

	t.Run("daemon", func(t *testing.T) {
		called := false
		cleanupOldExecutable = func() error {
			called = true
			return nil
		}
		os.Args = []string{"no-mistakes", "daemon", "log-sink", "--root", ""}
		if code := run(); code != 1 {
			t.Fatalf("run code = %d, want 1", code)
		}
		if !called {
			t.Fatal("cleanup was not attempted before daemon routing")
		}
	})

	t.Run("background update", func(t *testing.T) {
		cleanupFinished := false
		cleanupOldExecutable = func() error {
			cleanupFinished = true
			return fmt.Errorf("executable is still locked")
		}
		maybeHandleBackgroundCheck = func([]string) (bool, error) {
			if !cleanupFinished {
				t.Fatal("background routing ran before cleanup")
			}
			return true, nil
		}
		os.Args = []string{"no-mistakes", "--update-check", "v1.2.3"}
		if code := run(); code != 0 {
			t.Fatalf("run code = %d, want 0", code)
		}
	})

	t.Run("interactive", func(t *testing.T) {
		called := false
		cleanupOldExecutable = func() error {
			called = true
			return nil
		}
		maybeHandleBackgroundCheck = originalBackground
		os.Args = []string{"no-mistakes", "--version"}
		if code := run(); code != 0 {
			t.Fatalf("run code = %d, want 0", code)
		}
		if !called {
			t.Fatal("cleanup was not attempted before interactive routing")
		}
	})
}

// Issue #1653: `daemon run` must release a console of its own and repoint
// unusable standard output before the daemon spawns or writes anything, and
// against the root it was given, since the fallback writes to that root's
// bootstrap log.
func TestRunPreparesDaemonStdioBeforeTheDaemonRuns(t *testing.T) {
	originalArgs := os.Args
	originalPrepare := prepareDaemonRunStdio
	originalRunDaemon := runDaemon
	t.Cleanup(func() {
		os.Args = originalArgs
		prepareDaemonRunStdio = originalPrepare
		runDaemon = originalRunDaemon
	})
	root := t.TempDir()
	t.Setenv("NM_HOME", os.Getenv("NM_HOME"))

	var calls []string
	prepareDaemonRunStdio = func() error {
		calls = append(calls, "prepare NM_HOME="+os.Getenv("NM_HOME"))
		return fmt.Errorf("no usable bootstrap log")
	}
	runDaemon = func() error {
		calls = append(calls, "run")
		return nil
	}
	os.Args = []string{"no-mistakes", "daemon", "run", "--root", root}
	if code := run(); code != 0 {
		t.Fatalf("run code = %d, want 0 even when the best-effort preparation fails", code)
	}
	want := []string{"prepare NM_HOME=" + root, "run"}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
}

// The console release and the stdio fallback belong to `daemon run` alone:
// every other invocation, including the daemon's own log-sink child and a CLI
// sharing its console with a git hook, terminal, or agent, must keep its
// console and standard handles untouched.
func TestOnlyDaemonRunPreparesDaemonStdio(t *testing.T) {
	originalArgs := os.Args
	originalPrepare := prepareDaemonRunStdio
	originalRunDaemon := runDaemon
	originalBackground := maybeHandleBackgroundCheck
	t.Cleanup(func() {
		os.Args = originalArgs
		prepareDaemonRunStdio = originalPrepare
		runDaemon = originalRunDaemon
		maybeHandleBackgroundCheck = originalBackground
	})
	prepareDaemonRunStdio = func() error {
		t.Errorf("%q prepared daemon stdio", os.Args[1:])
		return nil
	}
	runDaemon = func() error {
		t.Errorf("%q ran the daemon", os.Args[1:])
		return nil
	}
	maybeHandleBackgroundCheck = func(args []string) (bool, error) {
		return len(args) > 0 && args[0] == "--update-check", nil
	}

	for _, args := range [][]string{
		{"--version"},
		{"daemon", "run", "--help"},
		{"daemon", "run", "--root"},
		{"daemon", "log-sink", "--root", ""},
		{"--update-check", "v1.2.3"},
	} {
		os.Args = append([]string{"no-mistakes"}, args...)
		_ = run()
	}

	// A real log-sink run: it reports ready on stdout and drains stdin to EOF.
	originalStdin, originalStdout := os.Stdin, os.Stdout
	t.Cleanup(func() {
		os.Stdin, os.Stdout = originalStdin, originalStdout
		signal.Reset(os.Interrupt, syscall.SIGTERM)
	})
	t.Setenv("NM_HOME", os.Getenv("NM_HOME"))
	stdin, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := stdinWriter.Close(); err != nil {
		t.Fatal(err)
	}
	stdout, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	os.Stdin, os.Stdout = stdin, stdout
	os.Args = []string{"no-mistakes", "daemon", "log-sink", "--root", t.TempDir()}
	code := run()
	os.Stdin, os.Stdout = originalStdin, originalStdout
	if code != 0 {
		t.Fatalf("log-sink run code = %d, want 0", code)
	}
	got, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "ready" {
		t.Fatalf("log-sink stdout = %q, want its ready line", got)
	}
}

func TestCLILogWriterReturnsDiscardWhenLogsDirMissing(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	w := cliLogWriter()
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(nmHome, "logs", "cli.log")); !os.IsNotExist(err) {
		t.Fatalf("cli.log should not be created when logs dir is missing, stat err = %v", err)
	}

	if c, ok := w.(io.Closer); ok {
		_ = c.Close()
	}
}

func TestCLILogWriterAppendsToFileWhenLogsDirExists(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	logsDir := filepath.Join(nmHome, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	w := cliLogWriter()
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if c, ok := w.(io.Closer); ok {
		if err := c.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	}

	b, err := os.ReadFile(filepath.Join(logsDir, "cli.log"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(b) != "hello\n" {
		t.Fatalf("cli.log contents = %q, want %q", string(b), "hello\n")
	}
}

func TestDaemonRunRootFromArgs(t *testing.T) {
	t.Setenv("NM_DAEMON", "")

	tests := []struct {
		name     string
		args     []string
		wantRoot string
		wantOK   bool
		wantErr  string
	}{
		{name: "non-daemon command", args: []string{"daemon", "status"}},
		{name: "daemon run no root", args: []string{"daemon", "run"}, wantOK: true},
		{name: "daemon run root flag", args: []string{"daemon", "run", "--root", "/tmp/nm"}, wantRoot: "/tmp/nm", wantOK: true},
		{name: "daemon run root equals", args: []string{"daemon", "run", "--root=/tmp/nm"}, wantRoot: "/tmp/nm", wantOK: true},
		{name: "daemon run missing root value", args: []string{"daemon", "run", "--root"}, wantErr: "missing value for --root"},
		{name: "daemon run help", args: []string{"daemon", "run", "--help"}},
		{name: "daemon run short help", args: []string{"daemon", "run", "-h"}},
		{name: "daemon run unknown flag", args: []string{"daemon", "run", "--bogus"}},
		{name: "daemon run extra arg", args: []string{"daemon", "run", "extra"}},
		{name: "daemon run root plus extra arg", args: []string{"daemon", "run", "--root", "/tmp/nm", "extra"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRoot, gotOK, err := daemonRunRootFromArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if gotRoot != tt.wantRoot || gotOK != tt.wantOK {
				t.Fatalf("got (%q, %v), want (%q, %v)", gotRoot, gotOK, tt.wantRoot, tt.wantOK)
			}
		})
	}
}

func TestDaemonLogSinkRootFromArgs(t *testing.T) {
	root, ok, err := daemonLogSinkRootFromArgs([]string{"daemon", "log-sink", "--root", "/tmp/nm"})
	if err != nil || !ok || root != "/tmp/nm" {
		t.Fatalf("got (%q, %v, %v)", root, ok, err)
	}
	for _, args := range [][]string{
		{"daemon", "status"},
		{"daemon", "log-sink"},
		{"daemon", "log-sink", "--root=/tmp/nm"},
	} {
		if root, ok, err := daemonLogSinkRootFromArgs(args); err != nil || ok || root != "" {
			t.Fatalf("%v got (%q, %v, %v)", args, root, ok, err)
		}
	}
}

func TestWriteDaemonRunErrorPreservesBootstrapSinkOwnership(t *testing.T) {
	root := t.TempDir()
	t.Setenv("NM_HOME", root)
	logDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bootstrapPath := filepath.Join(logDir, "daemon-bootstrap.log")
	const existing = "active output\n"
	if err := os.WriteFile(bootstrapPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := os.OpenFile(bootstrapPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	writeDaemonRunError(bootstrap, fmt.Errorf("startup rejected: %w", daemon.ErrSingletonLockHeld))
	if err := bootstrap.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(bootstrapPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != existing {
		t.Fatalf("bootstrap log = %q, want %q", got, existing)
	}

	terminalPath := filepath.Join(root, "terminal")
	terminal, err := os.Create(terminalPath)
	if err != nil {
		t.Fatal(err)
	}
	writeDaemonRunError(terminal, fmt.Errorf("startup rejected: %w", daemon.ErrSingletonLockHeld))
	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	terminalOutput, err := os.ReadFile(terminalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(terminalOutput), daemon.ErrSingletonLockHeld.Error()) {
		t.Fatalf("terminal output = %q", terminalOutput)
	}
}

func TestDaemonRunRootFromArgs_EnvDoesNotForceDaemonModeForProbes(t *testing.T) {
	t.Setenv("NM_DAEMON", "1")

	tests := [][]string{
		{"--version"},
		{"daemon", "status"},
		{"status"},
	}

	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			gotRoot, gotOK, err := daemonRunRootFromArgs(args)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if gotRoot != "" || gotOK {
				t.Fatalf("got (%q, %v), want (%q, %v)", gotRoot, gotOK, "", false)
			}
		})
	}
}
