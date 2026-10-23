package update

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestEnsureDaemonUsesCurrentExecutableAllowsWindowsCaseDifferences(t *testing.T) {
	origGOOS := currentGOOS
	origDaemonIsRunning := daemonIsRunning
	origDaemonExecutablePath := daemonExecutablePath
	t.Cleanup(func() {
		currentGOOS = origGOOS
		daemonIsRunning = origDaemonIsRunning
		daemonExecutablePath = origDaemonExecutablePath
	})

	currentGOOS = "windows"
	daemonIsRunning = func(*paths.Paths) (bool, error) {
		return true, nil
	}
	daemonExecutablePath = func(*paths.Paths) (string, error) {
		return `c:\program files\no-mistakes\NO-MISTAKES.exe`, nil
	}

	u := &updater{
		executablePath: `C:\Program Files\No-Mistakes\no-mistakes.exe`,
		paths:          paths.WithRoot(t.TempDir()),
	}

	if err := u.ensureDaemonUsesCurrentExecutable(); err != nil {
		t.Fatalf("ensureDaemonUsesCurrentExecutable error = %v", err)
	}
}

func TestDefaultResetDaemonReportsOfflineWhenRestartFails(t *testing.T) {
	origIsRunning := daemonIsRunning
	origStop := daemonStop
	origStart := daemonStart
	t.Cleanup(func() {
		daemonIsRunning = origIsRunning
		daemonStop = origStop
		daemonStart = origStart
	})

	checks := 0
	daemonIsRunning = func(*paths.Paths) (bool, error) {
		checks++
		if checks == 1 {
			return true, nil
		}
		return false, nil
	}
	daemonStop = func(*paths.Paths) error { return nil }
	daemonStart = func(*paths.Paths) error { return errors.New("boom") }

	err := defaultResetDaemon(&paths.Paths{}, false, "")
	if err == nil {
		t.Fatal("defaultResetDaemon should fail when restart fails")
	}
	var resetErr *daemonResetError
	if !errors.As(err, &resetErr) {
		t.Fatalf("expected daemonResetError, got %T", err)
	}
	if !resetErr.daemonOffline {
		t.Fatal("expected daemon to be marked offline")
	}
	if checks < 2 {
		t.Fatalf("expected follow-up daemon check, got %d checks", checks)
	}
	if !strings.Contains(err.Error(), "start daemon") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunningDaemonExecutablePathUsesPIDFile(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := os.WriteFile(p.PIDFile(), []byte(fmt.Sprintf("%d", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := runningDaemonExecutablePath(p)
	if err != nil {
		t.Fatalf("runningDaemonExecutablePath error = %v", err)
	}
	want, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got != resolveExecutablePath(want) {
		t.Fatalf("runningDaemonExecutablePath = %q, want %q", got, resolveExecutablePath(want))
	}
}

func TestRunningDaemonExecutablePathHandlesExecutablePathsWithSpaces(t *testing.T) {
	if os.Getenv("NO_MISTAKES_TEST_CHILD") == "1" {
		time.Sleep(10 * time.Second)
		return
	}

	originalPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "dir with spaces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(dir, "no mistakes test binary"+filepath.Ext(originalPath))
	if err := os.WriteFile(copyPath, binary, originalInfo.Mode().Perm()); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(copyPath, "-test.run=^TestRunningDaemonExecutablePathHandlesExecutablePathsWithSpaces$")
	cmd.Env = append(os.Environ(), "NO_MISTAKES_TEST_CHILD=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	p := paths.WithRoot(t.TempDir())
	if err := os.WriteFile(p.PIDFile(), []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := runningDaemonExecutablePath(p)
	if err != nil {
		t.Fatalf("runningDaemonExecutablePath error = %v", err)
	}
	if got != resolveExecutablePath(copyPath) {
		t.Fatalf("runningDaemonExecutablePath = %q, want %q", got, resolveExecutablePath(copyPath))
	}
}

func TestExecutablePathForPIDUsesWindowsResolver(t *testing.T) {
	origGOOS := currentGOOS
	origWindowsResolver := windowsExecutablePathForPID
	t.Cleanup(func() {
		currentGOOS = origGOOS
		windowsExecutablePathForPID = origWindowsResolver
	})

	currentGOOS = "windows"
	called := false
	windowsExecutablePathForPID = func(pid int) (string, error) {
		called = true
		if pid != 4321 {
			t.Fatalf("pid = %d, want %d", pid, 4321)
		}
		return `C:\Program Files\no-mistakes\no-mistakes.exe`, nil
	}

	got, err := executablePathForPID(4321)
	if err != nil {
		t.Fatalf("executablePathForPID error = %v", err)
	}
	if !called {
		t.Fatal("expected windows resolver to be used")
	}
	if got != `C:\Program Files\no-mistakes\no-mistakes.exe` {
		t.Fatalf("executablePathForPID = %q", got)
	}
}

func TestDefaultResetDaemonRecoversWhenHealthCheckErrors(t *testing.T) {
	origIsRunning := daemonIsRunning
	origStop := daemonStop
	origStart := daemonStart
	t.Cleanup(func() {
		daemonIsRunning = origIsRunning
		daemonStop = origStop
		daemonStart = origStart
	})

	stopCalled := false
	startCalled := false
	daemonIsRunning = func(*paths.Paths) (bool, error) { return false, errors.New("health check failed") }
	daemonStop = func(*paths.Paths) error {
		stopCalled = true
		return nil
	}
	daemonStart = func(*paths.Paths) error {
		startCalled = true
		return nil
	}

	if err := defaultResetDaemon(&paths.Paths{}, false, ""); err != nil {
		t.Fatalf("defaultResetDaemon error = %v", err)
	}
	if !stopCalled {
		t.Fatal("expected stop to be attempted after health-check error")
	}
	if !startCalled {
		t.Fatal("expected start to be attempted after health-check error")
	}
}

func TestDefaultResetDaemonNoopWhenDaemonOfflineAndNoArtifacts(t *testing.T) {
	origIsRunning := daemonIsRunning
	origStop := daemonStop
	origStart := daemonStart
	t.Cleanup(func() {
		daemonIsRunning = origIsRunning
		daemonStop = origStop
		daemonStart = origStart
	})

	p := paths.WithRoot(t.TempDir())
	stopCalled := false
	startCalled := false
	daemonIsRunning = func(*paths.Paths) (bool, error) { return false, nil }
	daemonStop = func(*paths.Paths) error {
		stopCalled = true
		return nil
	}
	daemonStart = func(*paths.Paths) error {
		startCalled = true
		return nil
	}

	if err := defaultResetDaemon(p, false, ""); err != nil {
		t.Fatalf("defaultResetDaemon error = %v", err)
	}
	if stopCalled {
		t.Fatal("expected stop to be skipped when daemon is offline without artifacts")
	}
	if startCalled {
		t.Fatal("expected start to be skipped when daemon is offline without artifacts")
	}
}

func TestDefaultResetDaemonRecoversWhenDaemonArtifactsRemain(t *testing.T) {
	origIsRunning := daemonIsRunning
	origStop := daemonStop
	origStart := daemonStart
	t.Cleanup(func() {
		daemonIsRunning = origIsRunning
		daemonStop = origStop
		daemonStart = origStart
	})

	p := paths.WithRoot(t.TempDir())
	if err := os.WriteFile(p.Socket(), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	stopCalled := false
	startCalled := false
	daemonIsRunning = func(*paths.Paths) (bool, error) { return false, nil }
	daemonStop = func(*paths.Paths) error {
		stopCalled = true
		return nil
	}
	daemonStart = func(*paths.Paths) error {
		startCalled = true
		return nil
	}

	if err := defaultResetDaemon(p, false, ""); err != nil {
		t.Fatalf("defaultResetDaemon error = %v", err)
	}
	if !stopCalled {
		t.Fatal("expected stop to be attempted when daemon artifacts remain")
	}
	if !startCalled {
		t.Fatal("expected start to be attempted when daemon artifacts remain")
	}
}

func TestDefaultResetDaemonDoesNotReportOfflineWhenRestartLeavesDaemonRunning(t *testing.T) {
	origIsRunning := daemonIsRunning
	origStop := daemonStop
	origStart := daemonStart
	t.Cleanup(func() {
		daemonIsRunning = origIsRunning
		daemonStop = origStop
		daemonStart = origStart
	})

	checks := 0
	daemonIsRunning = func(*paths.Paths) (bool, error) {
		checks++
		if checks == 1 {
			return true, nil
		}
		return true, nil
	}
	daemonStop = func(*paths.Paths) error { return nil }
	daemonStart = func(*paths.Paths) error { return errors.New("daemon already running") }

	err := defaultResetDaemon(&paths.Paths{}, false, "")
	if err == nil {
		t.Fatal("defaultResetDaemon should fail when restart fails")
	}
	var resetErr *daemonResetError
	if !errors.As(err, &resetErr) {
		t.Fatalf("expected daemonResetError, got %T", err)
	}
	if resetErr.daemonOffline {
		t.Fatal("expected daemon to stay online")
	}
	if checks < 2 {
		t.Fatalf("expected follow-up daemon check, got %d checks", checks)
	}
}

func TestDefaultResetDaemonStartsTheNewlyInstalledBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the installed binary")
	}
	origGOOS := currentGOOS
	origIsRunning := daemonIsRunning
	origStop := daemonStop
	origStart := daemonStart
	t.Cleanup(func() {
		currentGOOS = origGOOS
		daemonIsRunning = origIsRunning
		daemonStop = origStop
		daemonStart = origStart
	})

	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	exe := filepath.Join(dir, "no-mistakes")
	script := "#!/bin/sh\nprintf '%s ' \"$@\" > '" + argsFile + "'\necho \"$NO_MISTAKES_NO_UPDATE_CHECK\" >> '" + argsFile + "'\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			currentGOOS = goos
			daemonIsRunning = func(*paths.Paths) (bool, error) { return true, nil }
			daemonStop = func(*paths.Paths) error { return nil }
			daemonStart = func(*paths.Paths) error {
				t.Fatal("the old binary's in-process daemon.Start must not start the daemon after update")
				return nil
			}
			if err := defaultResetDaemon(&paths.Paths{}, false, exe); err != nil {
				t.Fatalf("defaultResetDaemon error = %v", err)
			}
			got, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("new binary was not run: %v", err)
			}
			if want := "daemon start 1\n"; string(got) != want {
				t.Fatalf("new binary ran with %q, want %q", got, want)
			}
		})
	}
}

func TestDefaultResetDaemonReportsTheNewBinarysStartFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the installed binary")
	}
	origGOOS := currentGOOS
	origIsRunning := daemonIsRunning
	origStop := daemonStop
	t.Cleanup(func() {
		currentGOOS = origGOOS
		daemonIsRunning = origIsRunning
		daemonStop = origStop
	})

	exe := filepath.Join(t.TempDir(), "no-mistakes")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho 'unit refresh failed' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	currentGOOS = "linux"
	checks := 0
	daemonIsRunning = func(*paths.Paths) (bool, error) {
		checks++
		return checks == 1, nil
	}
	daemonStop = func(*paths.Paths) error { return nil }

	err := defaultResetDaemon(&paths.Paths{}, false, exe)
	var resetErr *daemonResetError
	if !errors.As(err, &resetErr) || !resetErr.daemonOffline {
		t.Fatalf("expected an offline daemonResetError, got %v", err)
	}
	if !strings.Contains(err.Error(), "unit refresh failed") {
		t.Fatalf("error = %v, want the new binary's output", err)
	}
}

func TestDefaultResetDaemonKeepsInProcessStartOnWindows(t *testing.T) {
	origGOOS := currentGOOS
	origIsRunning := daemonIsRunning
	origStop := daemonStop
	origStart := daemonStart
	origStartExecutable := daemonStartExecutable
	t.Cleanup(func() {
		currentGOOS = origGOOS
		daemonIsRunning = origIsRunning
		daemonStop = origStop
		daemonStart = origStart
		daemonStartExecutable = origStartExecutable
	})

	currentGOOS = "windows"
	daemonIsRunning = func(*paths.Paths) (bool, error) { return true, nil }
	daemonStop = func(*paths.Paths) error { return nil }
	startCalled := false
	daemonStart = func(*paths.Paths) error { startCalled = true; return nil }
	daemonStartExecutable = func(string) error {
		t.Fatal("windows must keep the in-process start")
		return nil
	}
	if err := defaultResetDaemon(&paths.Paths{}, false, `C:\no-mistakes.exe`); err != nil {
		t.Fatalf("defaultResetDaemon error = %v", err)
	}
	if !startCalled {
		t.Fatal("expected the in-process start on windows")
	}
}
