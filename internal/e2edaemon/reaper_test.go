package e2edaemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// labRoot creates a mode-0700 isolation root under /private/tmp (or /tmp).
func labRoot(t *testing.T) string {
	t.Helper()
	base := "/tmp"
	if st, err := os.Stat("/private/tmp"); err == nil && st.IsDir() {
		base = "/private/tmp"
	}
	dir, err := os.MkdirTemp(base, "nm-e2e-own-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod lab: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func buildTestNMBin(t *testing.T) string {
	t.Helper()
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	outDir := t.TempDir()
	bin := filepath.Join(outDir, "no-mistakes")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/no-mistakes")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build no-mistakes: %v\n%s", err, out)
	}
	return bin
}

func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found from %s", wd)
		}
		dir = parent
	}
}

func startDetachedTestDaemon(t *testing.T, bin, nmHome string) int {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(nmHome, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "agent: claude\nlog_level: error\n"
	if err := os.WriteFile(filepath.Join(nmHome, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(nmHome, "logs", "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "daemon", "run", "--root", nmHome)
	cmd.Env = append(os.Environ(),
		"NM_HOME="+nmHome,
		"NM_TEST_START_DAEMON=1",
		"NO_MISTAKES_TELEMETRY=off",
		"NO_MISTAKES_NO_UPDATE_CHECK=1",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if runtime.GOOS != "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("start daemon: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	_ = logFile.Close()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if MatchesDaemonRoot(pid, nmHome) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			status := exec.CommandContext(ctx, bin, "daemon", "status")
			status.Env = append(os.Environ(),
				"NM_HOME="+nmHome,
				"NM_TEST_START_DAEMON=1",
				"NO_MISTAKES_TELEMETRY=off",
				"NO_MISTAKES_NO_UPDATE_CHECK=1",
			)
			out, _ := status.CombinedOutput()
			cancel()
			if strings.Contains(string(out), "running") {
				return pid
			}
		}
		if found, err := FindDaemonsForRoot(nmHome); err == nil {
			for _, p := range found {
				if MatchesDaemonRoot(p, nmHome) {
					pid = p
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("daemon for %s did not become ready (pid=%d); log:\n%s", nmHome, pid, readFileOrEmpty(logPath))
	return 0
}

func readFileOrEmpty(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func assertNoDaemonForRoot(t *testing.T, nmHome string) {
	t.Helper()
	found, err := FindDaemonsForRoot(nmHome)
	if err != nil {
		t.Fatalf("FindDaemonsForRoot: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("want zero daemons for %s, found pids %v", nmHome, found)
	}
}

func safetyReap(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if inv, err := Open(); err == nil {
			_ = inv.ReapAll()
		}
	})
}

func TestReapAll_NormalCompletion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process model")
	}
	lab := labRoot(t)
	t.Setenv(EnvInventory, filepath.Join(lab, "inv"))
	safetyReap(t)
	bin := buildTestNMBin(t)
	nmHome := filepath.Join(lab, "nm-e2e-normal", "nmhome")
	pid := startDetachedTestDaemon(t, bin, nmHome)

	inv, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := inv.Register("normal", nmHome, bin, pid, os.Getpid()); err != nil {
		t.Fatal(err)
	}

	// Happy path: reaper after explicit ownership release (daemon stop + kill match).
	result := inv.ReapAll()
	if result.Removed < 1 {
		t.Fatalf("reap result = %+v", result)
	}
	assertNoDaemonForRoot(t, nmHome)
	list, _ := inv.List()
	if len(list) != 0 {
		t.Fatalf("inventory not empty: %+v", list)
	}
}

func TestReapAll_CallerInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process model")
	}
	// Models SIGINT/TERM of the test driver: Cleanup never runs, inventory remains.
	lab := labRoot(t)
	t.Setenv(EnvInventory, filepath.Join(lab, "inv"))
	safetyReap(t)
	bin := buildTestNMBin(t)
	nmHome := filepath.Join(lab, "nm-e2e-interrupt", "nmhome")
	pid := startDetachedTestDaemon(t, bin, nmHome)

	inv, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := inv.Register("interrupt", nmHome, bin, pid, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if !MatchesDaemonRoot(pid, nmHome) {
		t.Fatalf("precondition: pid %d should match root", pid)
	}
	result := inv.ReapAll()
	if result.Removed < 1 {
		t.Fatalf("reap result = %+v", result)
	}
	assertNoDaemonForRoot(t, nmHome)
}

func TestReapAll_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process model")
	}
	// Models go test -timeout: process panics without Cleanup; inventory remains.
	lab := labRoot(t)
	t.Setenv(EnvInventory, filepath.Join(lab, "inv"))
	safetyReap(t)
	bin := buildTestNMBin(t)
	nmHome := filepath.Join(lab, "nm-e2e-timeout", "nmhome")
	pid := startDetachedTestDaemon(t, bin, nmHome)

	inv, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := inv.Register("timeout", nmHome, bin, pid, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	result := inv.ReapAll()
	if result.Removed < 1 {
		t.Fatalf("reap result = %+v", result)
	}
	assertNoDaemonForRoot(t, nmHome)
}

func TestReapAll_ChildSIGKILL_WrapperReaps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process model")
	}
	// Child starts a detached daemon, records inventory, then is SIGKILL'd.
	// Parent (suite wrapper role) reaps from on-disk inventory.
	// Honest: this does not model SIGKILL of the wrapper shell itself;
	// that path is next-run stale recovery (TestReapAll_StaleInventoryRecovery).
	if os.Getenv("E2EDAEMON_HELPER") == "sigkill-child" {
		runSigkillChildHelper()
		return
	}

	lab := labRoot(t)
	invDir := filepath.Join(lab, "inv")
	t.Setenv(EnvInventory, invDir)
	safetyReap(t)
	bin := buildTestNMBin(t)
	nmHome := filepath.Join(lab, "nm-e2e-sigkill", "nmhome")

	cmd := exec.Command(os.Args[0], "-test.run=^TestReapAll_ChildSIGKILL_WrapperReaps$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"E2EDAEMON_HELPER=sigkill-child",
		EnvInventory+"="+invDir,
		"E2EDAEMON_HELPER_BIN="+bin,
		"E2EDAEMON_HELPER_HOME="+nmHome,
	)
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}

	// Wait until inventory shows a live entry.
	deadline := time.Now().Add(45 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		inv, err := OpenDir(invDir)
		if err == nil {
			if list, err := inv.List(); err == nil && len(list) > 0 {
				// Prefer waiting until the daemon is actually live.
				if found, _ := FindDaemonsForRoot(nmHome); len(found) > 0 {
					ready = true
					break
				}
			}
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			t.Fatalf("helper exited early:\n%s", output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("helper never registered a live daemon:\n%s", output.String())
	}

	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL helper: %v", err)
	}
	_ = cmd.Wait()

	// Daemon must still be alive after child death (the leak class under test).
	found, err := FindDaemonsForRoot(nmHome)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatal("precondition failed: daemon already gone before wrapper reaper")
	}

	// Wrapper reaper.
	inv, err := OpenDir(invDir)
	if err != nil {
		t.Fatal(err)
	}
	result := inv.ReapAll()
	if result.Removed < 1 {
		t.Fatalf("wrapper reap result = %+v", result)
	}
	assertNoDaemonForRoot(t, nmHome)
}

func runSigkillChildHelper() {
	bin := os.Getenv("E2EDAEMON_HELPER_BIN")
	nmHome := os.Getenv("E2EDAEMON_HELPER_HOME")
	invDir := os.Getenv(EnvInventory)
	if bin == "" || nmHome == "" || invDir == "" {
		fmt.Fprintln(os.Stderr, "missing helper env")
		os.Exit(1)
	}
	// Inline start (no testing.T).
	_ = os.MkdirAll(filepath.Join(nmHome, "logs"), 0o755)
	_ = os.WriteFile(filepath.Join(nmHome, "config.yaml"), []byte("agent: claude\nlog_level: error\n"), 0o644)
	logFile, err := os.OpenFile(filepath.Join(nmHome, "logs", "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cmd := exec.Command(bin, "daemon", "run", "--root", nmHome)
	cmd.Env = append(os.Environ(),
		"NM_HOME="+nmHome,
		"NM_TEST_START_DAEMON=1",
		"NO_MISTAKES_TELEMETRY=off",
		"NO_MISTAKES_NO_UPDATE_CHECK=1",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	_ = logFile.Close()

	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if MatchesDaemonRoot(pid, nmHome) {
			break
		}
		if found, _ := FindDaemonsForRoot(nmHome); len(found) > 0 {
			pid = found[0]
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	inv, err := OpenDir(invDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := inv.Register("child", nmHome, bin, pid, os.Getpid()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("ready")
	select {}
}

func TestReapAll_StaleInventoryRecovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process model")
	}
	// Next-run recovery: inventory has a dead PID but a live daemon under home.
	// Also covers "wrapper shell SIGKILL'd" honesty — on-disk inventory remains.
	lab := labRoot(t)
	t.Setenv(EnvInventory, filepath.Join(lab, "inv"))
	safetyReap(t)
	bin := buildTestNMBin(t)
	nmHome := filepath.Join(lab, "nm-e2e-stale", "nmhome")
	_ = startDetachedTestDaemon(t, bin, nmHome)

	inv, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := inv.Register("stale", nmHome, bin, 999999, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	result := inv.ReapAll()
	if result.Removed < 1 {
		t.Fatalf("reap result = %+v", result)
	}
	assertNoDaemonForRoot(t, nmHome)
}

func TestReapAll_DoesNotTouchSharedDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process model")
	}
	lab := labRoot(t)
	t.Setenv(EnvInventory, filepath.Join(lab, "inv"))

	sharedHome := filepath.Join(mustUserHome(t), ".no-mistakes")
	before, _ := FindDaemonsForRoot(sharedHome)

	inv, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	tempHome := filepath.Join(lab, "nm-e2e-safe", "nmhome")
	_ = os.MkdirAll(tempHome, 0o700)
	if err := inv.Register("safe", tempHome, "", 0, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	_ = inv.ReapAll()

	after, _ := FindDaemonsForRoot(sharedHome)
	if len(before) != len(after) {
		t.Fatalf("shared daemon pids changed: before=%v after=%v", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("shared daemon pids changed: before=%v after=%v", before, after)
		}
	}
	// Shared service still healthy if it was running.
	if len(before) > 0 {
		alive, err := processAlive(before[0])
		if err != nil || !alive {
			t.Fatalf("shared daemon pid %d not alive after reap", before[0])
		}
	}
}

func mustUserHome(t *testing.T) string {
	t.Helper()
	h, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return h
}
