//go:build windows

package shellenv

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"golang.org/x/sys/windows"
)

// TestIsTaskkillAlreadyGone pins down the locale-independent contract the
// Windows cancel path relies on: taskkill exit code 128 (no matching PID) is
// the only nonzero exit treated as "the child already exited", so every other
// nonzero code falls through to the direct-child-kill backstop instead of
// being swallowed as os.ErrProcessDone. It runs only on Windows; on Linux the
// windows build tag excludes it from `go test ./...`, while `GOOS=windows go
// vet` keeps it compile-checked.
func TestIsTaskkillAlreadyGone(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "exit 128 is already gone", err: exitCodeErr(t, 128), want: true},
		{name: "exit 1 access denied is a real failure", err: exitCodeErr(t, 1), want: false},
		{name: "exec.ErrNotFound is not already-gone", err: exec.ErrNotFound, want: false},
		{name: "wrapped exit 128 still detected", err: wrapErr(exitCodeErr(t, 128)), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTaskkillAlreadyGone(tt.err); got != tt.want {
				t.Fatalf("isTaskkillAlreadyGone(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestStartShellCommandFailsWhenJobSetupFails(t *testing.T) {
	setupErr := errors.New("job setup denied")
	oldNewJob := newShellCommandJobFunc
	newShellCommandJobFunc = func() (windows.Handle, error) {
		return 0, setupErr
	}
	t.Cleanup(func() {
		newShellCommandJobFunc = oldNewJob
	})

	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit", "0")
	ConfigureShellCommand(cmd)
	if _, ok := shellCommandJob(cmd); ok {
		t.Fatal("expected no job state when job setup fails")
	}
	err := StartShellCommand(cmd)
	if !errors.Is(err, setupErr) {
		t.Fatalf("StartShellCommand() error = %v, want setup error", err)
	}
	if cmd.Process != nil {
		t.Fatal("expected command not to start after job setup failure")
	}
}

func TestConfigureCooperativeShellCommandCancelAllowsCleanup(t *testing.T) {
	installWindowsConsoleInterruptTestSender(t)

	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	cleanup := filepath.Join(dir, "cleanup")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := windowsCommandCancellationHelper(ctx)
	cmd.Env = append(os.Environ(),
		"NM_WINDOWS_COMMAND_HELPER=cooperate",
		"NM_WINDOWS_HELPER_EXE="+os.Args[0],
		"NM_WINDOWS_COMMAND_READY="+ready,
		"NM_WINDOWS_COMMAND_CLEANUP="+cleanup,
	)
	ConfigureCooperativeShellCommand(cmd)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	done := make(chan error, 1)
	go func() { done <- RunShellCommand(cmd) }()
	childPID := waitForWindowsHelperPID(t, ready, done, &output)
	if childPID == cmd.Process.Pid {
		t.Fatalf("helper PID = shell PID %d; want a real cmd.exe descendant", childPID)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cooperating command did not exit after cancellation")
	}
	if _, err := os.Stat(cleanup); err != nil {
		t.Fatalf("cooperative cleanup side effect missing: %v", err)
	}
}

func TestConfigureCooperativeShellCommandCancelForcesNonCooperatingTree(t *testing.T) {
	installWindowsConsoleInterruptTestSender(t)

	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := windowsCommandCancellationHelper(ctx)
	cmd.Env = append(os.Environ(),
		"NM_WINDOWS_COMMAND_HELPER=ignore",
		"NM_WINDOWS_HELPER_EXE="+os.Args[0],
		"NM_WINDOWS_COMMAND_READY="+ready,
	)
	ConfigureCooperativeShellCommand(cmd)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	done := make(chan error, 1)
	go func() { done <- RunShellCommand(cmd) }()
	childPID := waitForWindowsHelperPID(t, ready, done, &output)
	if childPID == cmd.Process.Pid {
		t.Fatalf("helper PID = shell PID %d; want a real cmd.exe descendant", childPID)
	}
	started := time.Now()
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("noncooperating command tree survived forced cancellation")
	}
	if elapsed := time.Since(started); elapsed < windowsTerminateGrace {
		t.Fatalf("forced cancellation took %s, want at least cooperative grace %s", elapsed, windowsTerminateGrace)
	}
}

func TestWindowsCommandCancellationHelper(t *testing.T) {
	mode := os.Getenv("NM_WINDOWS_COMMAND_HELPER")
	if mode == "" {
		return
	}
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	if err := os.WriteFile(os.Getenv("NM_WINDOWS_COMMAND_READY"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if mode == "ignore" {
		for range interrupts {
		}
	}
	<-interrupts
	if err := os.WriteFile(os.Getenv("NM_WINDOWS_COMMAND_CLEANUP"), []byte("cleaned"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsConsoleInterruptSenderHelper(t *testing.T) {
	pidText := os.Getenv("NM_WINDOWS_CONSOLE_TARGET_PID")
	if pidText == "" {
		return
	}
	pid, err := strconv.ParseUint(pidText, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	if err := generateWindowsConsoleInterrupt(uint32(pid)); err != nil {
		t.Fatal(err)
	}
}

func installWindowsConsoleInterruptTestSender(t *testing.T) {
	t.Helper()
	old := sendWindowsConsoleInterruptFunc
	sendWindowsConsoleInterruptFunc = func(pid uint32) error {
		cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsConsoleInterruptSenderHelper$")
		cmd.Env = append(os.Environ(), "NM_WINDOWS_CONSOLE_TARGET_PID="+strconv.FormatUint(uint64(pid), 10))
		winproc.Harden(cmd)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return errors.New(string(output))
		}
		return nil
	}
	t.Cleanup(func() { sendWindowsConsoleInterruptFunc = old })
}

func windowsCommandCancellationHelper(ctx context.Context) *exec.Cmd {
	// Configured Windows commands use this exact shell topology. Keeping cmd.exe
	// as the job leader proves the control event reaches a real descendant and
	// that an early shell exit cannot cut the descendant's cleanup window short.
	return exec.CommandContext(ctx, "cmd.exe", "/d", "/c",
		`%NM_WINDOWS_HELPER_EXE% -test.run=TestWindowsCommandCancellationHelper`)
}

func waitForWindowsHelperPID(t *testing.T, path string, done <-chan error, output *bytes.Buffer) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("cmd.exe helper exited before ready: %v\n%s", err, output.String())
		default:
		}
		contents, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(string(contents))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("helper readiness PID file %s was not created", path)
	return 0
}

func TestStartShellCommandFailsWhenJobAssignmentFails(t *testing.T) {
	assignmentErr := errors.New("assignment denied")
	oldAssign := assignShellCommandJobFunc
	assignShellCommandJobFunc = func(windows.Handle, uint32) error {
		return assignmentErr
	}
	t.Cleanup(func() {
		assignShellCommandJobFunc = oldAssign
	})

	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit", "0")
	ConfigureShellCommand(cmd)
	if _, ok := shellCommandJob(cmd); !ok {
		t.Skip("job object setup unavailable")
	}
	err := StartShellCommand(cmd)
	if !errors.Is(err, assignmentErr) {
		t.Fatalf("StartShellCommand() error = %v, want assignment error", err)
	}
	if _, ok := shellCommandJob(cmd); ok {
		t.Fatal("expected failed job state to be closed")
	}
	if cmd.Process == nil {
		t.Fatal("expected command to have started before assignment failure")
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatal("expected failed start to kill and wait for the suspended process")
	}
}

// exitCodeErr runs `cmd /c exit N` and returns the resulting *exec.ExitError so
// the helper is exercised against a real ProcessState with the chosen code.
func exitCodeErr(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("cmd", "/c", "exit", strconv.Itoa(code)).Run()
	if err == nil {
		t.Fatalf("expected exit %d to yield a nonzero-run error", code)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
	if exitErr.ExitCode() != code {
		t.Fatalf("exit code = %d, want %d", exitErr.ExitCode(), code)
	}
	return err
}

type wrappedErr struct{ e error }

func (w wrappedErr) Error() string { return "wrapped: " + w.e.Error() }
func (w wrappedErr) Unwrap() error { return w.e }

func wrapErr(e error) error { return wrappedErr{e: e} }
