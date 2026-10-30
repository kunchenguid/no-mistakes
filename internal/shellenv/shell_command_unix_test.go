//go:build unix

package shellenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestShellCommandGroupActiveIgnoresZombiesButKeepsLiveChildren(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(strconv.FormatBool(live), func(t *testing.T) {
			leader := exec.CommandContext(context.Background(), "/bin/sh", "-c", "read release")
			ConfigureShellCommand(leader)
			stdin, err := leader.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := StartShellCommand(leader); err != nil {
				t.Fatal(err)
			}
			defer TerminateShellCommandGroup(leader)
			defer stdin.Close()
			// The test owns the child's wait, so the exited group member stays a
			// zombie until this test reaps it. No orphan/init timing is involved.
			zombie := exec.Command("/bin/sh", "-c", "exit 0")
			zombie.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.Process.Pid}
			if err := zombie.Start(); err != nil {
				t.Fatal(err)
			}
			defer zombie.Wait()
			deadline := time.Now().Add(5 * time.Second)
			for {
				out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(zombie.Process.Pid)).Output()
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("child did not become a zombie")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if live {
				child := exec.Command("sleep", "30")
				child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.Process.Pid}
				if err := child.Start(); err != nil {
					t.Fatal(err)
				}
				defer child.Wait()
				defer child.Process.Kill()
			}
			if _, err := stdin.Write([]byte("release\n")); err != nil {
				t.Fatal(err)
			}
			if err := leader.Wait(); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(-leader.Process.Pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
				t.Fatalf("fixture group must still exist: %v", err)
			}
			active, _, err := ShellCommandGroupActive(leader, 300*time.Millisecond)
			if err != nil || active != live {
				t.Fatalf("live=%v: group active=%v, error=%v", live, active, err)
			}
		})
	}
}

// TestShellCommandGroupActiveAcceptsMemberFinishingInsideWindow is the issue
// #1614 regression: a member still finishing when the leader exits is seen by
// the first probe, but it leaves the group inside the settle window, so the
// bounded re-probe accepts the leader's exit. The same member probed with no
// window is still refused, which is what the check did before the fix.
func TestShellCommandGroupActiveAcceptsMemberFinishingInsideWindow(t *testing.T) {
	// The member is a direct backgrounded process (not a subshell) with its
	// stdio detached, so it outlives the leader by ~0.5s inside the leader's
	// process group and reparents to init - the exact shape the wild failures
	// showed.
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "sleep 0.5 >/dev/null 2>&1 & exit 0")
	ConfigureShellCommand(cmd)
	if err := StartShellCommand(cmd); err != nil {
		t.Fatal(err)
	}
	defer TerminateShellCommandGroup(cmd)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}

	active, detail, err := ShellCommandGroupActive(cmd, 0)
	if err != nil || !active {
		t.Fatalf("single probe: active=%v detail=%q err=%v, want the finishing member reported", active, detail, err)
	}
	if !strings.Contains(detail, "(sleep 0.5)") {
		t.Fatalf("single probe detail = %q, want the member's pid and command", detail)
	}

	started := time.Now()
	active, detail, err = ShellCommandGroupActive(cmd, 3*time.Second)
	if err != nil || active {
		t.Fatalf("bounded re-probe: active=%v detail=%q err=%v, want the finishing member accepted", active, detail, err)
	}
	if elapsed := time.Since(started); elapsed >= 3*time.Second {
		t.Fatalf("bounded re-probe took %s; the member left the group inside the window", elapsed)
	}
}

// TestShellCommandGroupActiveRefusesMemberSurvivingWindow keeps the check
// fail-closed under the re-probe: a member that is still alive when the window
// ends is refused, and the verdict names it so the failure identifies the
// offending process.
func TestShellCommandGroupActiveRefusesMemberSurvivingWindow(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "member.pid")
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c",
		"sleep 30 >/dev/null 2>&1 & echo $! > "+pidFile+"; exit 0")
	ConfigureShellCommand(cmd)
	if err := StartShellCommand(cmd); err != nil {
		t.Fatal(err)
	}
	defer TerminateShellCommandGroup(cmd)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	memberPID := readPID(t, pidFile, 5*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(memberPID, syscall.SIGKILL) })

	const window = 400 * time.Millisecond
	started := time.Now()
	active, detail, err := ShellCommandGroupActive(cmd, window)
	if err != nil || !active {
		t.Fatalf("active=%v detail=%q err=%v, want the surviving member refused", active, detail, err)
	}
	if elapsed := time.Since(started); elapsed < window {
		t.Fatalf("re-probe returned after %s, before the %s window elapsed", elapsed, window)
	}
	if !strings.Contains(detail, "pid "+strconv.Itoa(memberPID)) || !strings.Contains(detail, "(sleep 30)") {
		t.Fatalf("detail = %q, want pid %d and its command named", detail, memberPID)
	}
}

// TestShellCommandGroupActivePassesImmediatelyForSessionEscapee documents the
// scope boundary: a member that calls setsid leaves the leader's process group
// and is invisible to the probe - the verdict passes without spending the
// settle window. (Escaped orphans are internal/procreap's job, not this
// check's.)
func TestShellCommandGroupActivePassesImmediatelyForSessionEscapee(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "member.pid")
	readyFile := filepath.Join(dir, "member.ready")
	leader := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestShellCommandSessionEscapeeHelper$")
	leader.Env = append(os.Environ(),
		"NM_SHELLENV_SESSION_HELPER=leader",
		"NM_SHELLENV_SESSION_PID="+pidFile,
		"NM_SHELLENV_SESSION_READY="+readyFile,
	)
	ConfigureShellCommand(leader)
	if err := StartShellCommand(leader); err != nil {
		t.Fatal(err)
	}
	defer TerminateShellCommandGroup(leader)
	memberPID := readPID(t, pidFile, 5*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(memberPID, syscall.SIGKILL) })
	if err := leader.Wait(); err != nil {
		t.Fatal(err)
	}

	const window = 2 * time.Second
	started := time.Now()
	active, detail, err := ShellCommandGroupActive(leader, window)
	if err != nil || active {
		t.Fatalf("active=%v detail=%q err=%v, want the escaped member invisible", active, detail, err)
	}
	if elapsed := time.Since(started); elapsed >= window {
		t.Fatalf("escapee was probed for the whole %s window; it should pass immediately", window)
	}
}

// TestShellCommandSessionEscapeeHelper is the process tree for
// TestShellCommandGroupActivePassesImmediatelyForSessionEscapee: the leader
// spawns a child that calls setsid(2) to leave the leader's process group,
// reports that through the ready file, and only then exits - so the probe runs
// after the member is already gone from the group.
func TestShellCommandSessionEscapeeHelper(t *testing.T) {
	switch os.Getenv("NM_SHELLENV_SESSION_HELPER") {
	case "leader":
		child := exec.Command(os.Args[0], "-test.run=^TestShellCommandSessionEscapeeHelper$")
		child.Env = append(os.Environ(),
			"NM_SHELLENV_SESSION_HELPER=escapee",
			"NM_SHELLENV_SESSION_READY="+os.Getenv("NM_SHELLENV_SESSION_READY"),
		)
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv("NM_SHELLENV_SESSION_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o644); err != nil {
			_ = child.Process.Kill()
			os.Exit(4)
		}
		if !waitForHelperReady(os.Getenv("NM_SHELLENV_SESSION_READY"), 5*time.Second) {
			_ = child.Process.Kill()
			os.Exit(3)
		}
		os.Exit(0)
	case "escapee":
		if _, err := syscall.Setsid(); err != nil {
			os.Exit(5)
		}
		if err := os.WriteFile(os.Getenv("NM_SHELLENV_SESSION_READY"), []byte("ready"), 0o644); err != nil {
			os.Exit(6)
		}
		time.Sleep(30 * time.Second)
		os.Exit(0)
	default:
		t.Skip("helper invoked by TestShellCommandGroupActivePassesImmediatelyForSessionEscapee")
	}
}

// TestTerminateShellCommandGroup_ReapsGrandchildAfterCleanExit pins the
// success-path guarantee that keeps the daemon alive: when a leader configured
// with ConfigureShellCommand exits 0 but leaves a grandchild alive in its
// process group (a test runner's worker pool), TerminateShellCommandGroup
// SIGKILLs the whole group. cmd.Cancel only fires on cancellation, so without
// this the grandchild leaks and orphan pools pile up across runs until the host
// OOMs and the OS kills the daemon.
func TestTerminateShellCommandGroup_ReapsGrandchildAfterCleanExit(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	// The leader backgrounds a long-lived grandchild (stdio detached so it does
	// not hold the inherited pipes open), records its pid, and exits 0.
	script := "( sleep 120 >/dev/null 2>&1 ) & echo $! > " + pidFile + "; exit 0"
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", script)
	ConfigureShellCommand(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("leader Run: %v", err)
	}

	grandchild := readPID(t, pidFile, 5*time.Second)
	if syscall.Kill(grandchild, 0) != nil {
		t.Fatalf("precondition failed: grandchild %d should still be alive before reap", grandchild)
	}

	TerminateShellCommandGroup(cmd)

	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild %d still alive after TerminateShellCommandGroup; group leaked", grandchild)
	}
}

// TestTerminateShellCommandGroup_AsksBeforeKilling pins that a surviving
// group member is given the chance to shut down: it receives SIGTERM and its
// handler runs to completion. SIGKILL would deny a test runner or worker
// script the chance to flush output and clean up its own temporary state.
//
// A /bin/sh grandchild with `trap` and `sleep` is not this contract: macOS
// /bin/sh (bash 3.2) delivers process-group SIGTERM to both the shell and its
// sleep child, then exits without running the trap. The ready-file handshake
// only proved trap was registered, which is why this test still failed in
// 0.01s on CI run 31827318230 after that fix. The grandchild is a Go helper
// that uses signal.Notify, so SIGTERM is observed by this process, not a
// sleep(1) that dies with the default action.
func TestTerminateShellCommandGroup_AsksBeforeKilling(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	termFile := filepath.Join(dir, "grandchild.term")
	readyFile := filepath.Join(dir, "grandchild.ready")

	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestTerminateShellCommandGroupTermHelper$")
	cmd.Env = append(os.Environ(),
		"NM_SHELLENV_TERM_HELPER=leader",
		"NM_SHELLENV_TERM_PID="+pidFile,
		"NM_SHELLENV_TERM_READY="+readyFile,
		"NM_SHELLENV_TERM_FILE="+termFile,
	)
	ConfigureShellCommand(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("leader Run: %v", err)
	}
	grandchild := readPID(t, pidFile, 5*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })

	TerminateShellCommandGroup(cmd)

	if !pidGoneWithin(grandchild, 5*time.Second) {
		t.Fatalf("grandchild %d still alive after TerminateShellCommandGroup", grandchild)
	}
	if _, err := os.Stat(termFile); err != nil {
		t.Fatalf("grandchild never ran its SIGTERM handler: %v", err)
	}
}

func TestTerminateShellCommandGroupTermHelper(t *testing.T) {
	switch os.Getenv("NM_SHELLENV_TERM_HELPER") {
	case "leader":
		child := exec.Command(os.Args[0], "-test.run=^TestTerminateShellCommandGroupTermHelper$")
		child.Env = append(os.Environ(),
			"NM_SHELLENV_TERM_HELPER=grandchild",
			"NM_SHELLENV_TERM_PID="+os.Getenv("NM_SHELLENV_TERM_PID"),
			"NM_SHELLENV_TERM_READY="+os.Getenv("NM_SHELLENV_TERM_READY"),
			"NM_SHELLENV_TERM_FILE="+os.Getenv("NM_SHELLENV_TERM_FILE"),
		)
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if !waitForHelperReady(os.Getenv("NM_SHELLENV_TERM_READY"), 5*time.Second) {
			_ = child.Process.Kill()
			os.Exit(3)
		}
		os.Exit(0)
	case "grandchild":
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGTERM)
		if err := os.WriteFile(os.Getenv("NM_SHELLENV_TERM_PID"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			os.Exit(4)
		}
		if err := os.WriteFile(os.Getenv("NM_SHELLENV_TERM_READY"), []byte("ready"), 0o644); err != nil {
			os.Exit(5)
		}
		<-term
		if err := os.WriteFile(os.Getenv("NM_SHELLENV_TERM_FILE"), []byte("terminated"), 0o644); err != nil {
			os.Exit(6)
		}
		os.Exit(0)
	default:
		t.Skip("helper invoked by TestTerminateShellCommandGroup_AsksBeforeKilling")
	}
}

// TestTerminateShellCommandGroup_EscalatesWhenSIGTERMIsIgnored is the other
// half of the contract: politeness must not become a new way to leak. A group
// member that ignores SIGTERM is SIGKILLed once the grace period is up.
func TestTerminateShellCommandGroup_EscalatesWhenSIGTERMIsIgnored(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")

	script := "( trap '' TERM; while :; do sleep 0.1; done ) >/dev/null 2>&1 & echo $! > " + pidFile + "; exit 0"
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", script)
	ConfigureShellCommand(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("leader Run: %v", err)
	}
	grandchild := readPID(t, pidFile, 5*time.Second)

	TerminateShellCommandGroup(cmd)

	if !pidGoneWithin(grandchild, 5*time.Second) {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild %d ignored SIGTERM and was never escalated to SIGKILL", grandchild)
	}
}

// TestConfigureShellCommand_CancelEscalatesWithoutBlockingWait pins the
// cancellation path: cmd.Cancel runs on the goroutine that owns cmd.Wait, so
// it must return promptly rather than sit through the grace period, and the
// SIGKILL escalation still has to land on a group member that ignores
// SIGTERM.
func TestConfigureShellCommand_CancelEscalatesWithoutBlockingWait(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	script := "( trap '' TERM; while :; do sleep 0.1; done ) >/dev/null 2>&1 & echo $! > " + pidFile + "; " +
		"trap '' TERM; while :; do sleep 0.1; done"
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	ConfigureShellCommand(cmd)
	if err := StartShellCommand(cmd); err != nil {
		t.Fatalf("StartShellCommand: %v", err)
	}
	grandchild := readPID(t, pidFile, 5*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })

	cancel()
	_ = cmd.Wait()

	if !pidGoneWithin(grandchild, 10*time.Second) {
		t.Fatalf("grandchild %d survived cancellation", grandchild)
	}
}

// TestTerminateShellCommandGroup_NoopOnNilOrUnstarted guards the cheap safety
// contract: a nil command, or one that was never started (no Process), must be
// a no-op rather than panic or signal an arbitrary pid.
func TestTerminateShellCommandGroup_NoopOnNilOrUnstarted(t *testing.T) {
	TerminateShellCommandGroup(nil)
	cmd := exec.Command("/bin/sh", "-c", "true") // never Start()ed: cmd.Process is nil
	TerminateShellCommandGroup(cmd)
}

func TestCombinedOutputShellCommand_ReturnsCleanExitWithInheritedPipeGrandchild(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "printf 'leader done\\n'; sleep 30 & exit 0")
	ConfigureShellCommand(cmd)
	cmd.WaitDelay = 100 * time.Millisecond

	out, err := CombinedOutputShellCommand(cmd)
	if err != nil {
		t.Fatalf("CombinedOutputShellCommand() error = %v; output %q", err, out)
	}
	if got, want := string(out), "leader done\n"; got != want {
		t.Fatalf("CombinedOutputShellCommand() output = %q, want %q", got, want)
	}
}

func TestCombinedOutputShellCommand_WaitDelayBoundsEscapedPipeHolder(t *testing.T) {
	readyFile := filepath.Join(t.TempDir(), "ready")
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestShellOutputPipeHelper$")
	cmd.Env = append(os.Environ(),
		"NM_SHELLENV_PIPE_HELPER=leader",
		"NM_SHELLENV_PIPE_READY="+readyFile,
	)
	ConfigureShellCommand(cmd)
	cmd.WaitDelay = 100 * time.Millisecond

	out, err := CombinedOutputShellCommand(cmd)
	escapedPID := parseEscapedPID(t, string(out))
	t.Cleanup(func() {
		_ = syscall.Kill(escapedPID, syscall.SIGKILL)
	})
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("CombinedOutputShellCommand() error = %v, want %v; output %q", err, exec.ErrWaitDelay, out)
	}
	if !strings.Contains(string(out), "leader done\n") {
		t.Fatalf("CombinedOutputShellCommand() output = %q, want leader output", out)
	}
}

func TestShellOutputPipeHelper(t *testing.T) {
	switch os.Getenv("NM_SHELLENV_PIPE_HELPER") {
	case "leader":
		child := exec.Command(os.Args[0], "-test.run=^TestShellOutputPipeHelper$")
		child.Env = append(os.Environ(),
			"NM_SHELLENV_PIPE_HELPER=escaped",
			"NM_SHELLENV_PIPE_READY="+os.Getenv("NM_SHELLENV_PIPE_READY"),
		)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if !waitForHelperReady(os.Getenv("NM_SHELLENV_PIPE_READY"), 5*time.Second) {
			os.Exit(3)
		}
		_, _ = os.Stdout.WriteString("leader done\nescaped pid " + strconv.Itoa(child.Process.Pid) + "\n")
		os.Exit(0)
	case "escaped":
		_, _ = syscall.Setsid()
		_ = os.WriteFile(os.Getenv("NM_SHELLENV_PIPE_READY"), []byte("ready"), 0o644)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}

func readPID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if v, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil && v > 0 {
				return v
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for a pid in %s", path)
	return 0
}

func parseEscapedPID(t *testing.T, output string) int {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "escaped pid ") {
			pid, err := strconv.Atoi(strings.TrimPrefix(line, "escaped pid "))
			if err == nil && pid > 0 {
				return pid
			}
		}
	}
	t.Fatalf("output %q did not contain escaped pid", output)
	return 0
}

func waitForHelperReady(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func pidGoneWithin(pid int, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) == syscall.ESRCH
}
