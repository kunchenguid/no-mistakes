//go:build unix

package procreap

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// TestSweepReapsSetsidEscapeeThatProcessGroupTeardownCannotReach is the
// end-to-end reproduction of the leak this package exists for, with real
// processes. A pipeline child calls setsid(2), leaving the process group
// no-mistakes isolated it in; the group teardown that runs on every exit path
// then cannot reach it, and once its parent exits it reparents to init and
// runs forever. The test asserts both halves: the escapee genuinely survives
// the group teardown, and the worktree-scoped sweep - the one the daemon runs
// when a run's goroutine finishes - terminates it.
func TestSweepReapsSetsidEscapeeThatProcessGroupTeardownCannotReach(t *testing.T) {
	requireCWDLookup(t)
	root, wt := newFakeWorktree(t)

	escapee := startEscapeeUnderLeader(t, wt)
	if !processAlive(escapee) {
		t.Fatalf("precondition failed: escapee %d should have survived the process-group teardown", escapee)
	}
	if ppid := parentOf(t, escapee); ppid > 1 {
		t.Fatalf("precondition failed: escapee %d should have reparented to init, has ppid %d", escapee, ppid)
	}

	victims, err := Sweep(Options{WorktreesRoot: root, Scope: wt, Grace: 5 * time.Second})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !containsPID(victims, escapee) {
		t.Fatalf("Sweep victims = %v, want it to include escapee %d", victimPIDs(victims), escapee)
	}
	if !pidGoneWithin(escapee, 5*time.Second) {
		t.Fatalf("escapee %d still alive after Sweep", escapee)
	}
}

// TestSweepReapsStaleOrphanFromDaemonStartupShape exercises the unscoped call
// the daemon makes on startup - the path that has to clean up after a
// predecessor daemon that died without ever running its own teardown.
func TestSweepReapsStaleOrphanFromDaemonStartupShape(t *testing.T) {
	requireCWDLookup(t)
	root, wt := newFakeWorktree(t)

	escapee := startEscapeeUnderLeader(t, wt)
	if !processAlive(escapee) {
		t.Fatalf("precondition failed: escapee %d should still be alive", escapee)
	}

	victims, err := Sweep(Options{
		WorktreesRoot: root,
		MinAge:        0,
		RunActive:     func(_, _ string) bool { return false },
		Grace:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !containsPID(victims, escapee) {
		t.Fatalf("Sweep victims = %v, want it to include escapee %d", victimPIDs(victims), escapee)
	}
	if !pidGoneWithin(escapee, 5*time.Second) {
		t.Fatalf("escapee %d still alive after Sweep", escapee)
	}
}

// TestProcReapHelper is not a test: it is the re-executed helper the process
// tests above drive through NM_PROCREAP_HELPER. It exits before the testing
// framework can report anything.
func TestProcReapHelper(t *testing.T) {
	switch os.Getenv("NM_PROCREAP_HELPER") {
	case "leader":
		child := exec.Command(os.Args[0], "-test.run=^TestProcReapHelper$")
		child.Env = append(os.Environ(),
			"NM_PROCREAP_HELPER=escaped",
			"NM_PROCREAP_READY="+os.Getenv("NM_PROCREAP_READY"),
		)
		child.Dir = os.Getenv("NM_PROCREAP_WORKDIR")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if !waitForFile(os.Getenv("NM_PROCREAP_READY"), 10*time.Second) {
			os.Exit(3)
		}
		_, _ = os.Stdout.WriteString("escaped pid " + strconv.Itoa(child.Process.Pid) + "\n")
		os.Exit(0)
	case "escaped":
		// Leaving the session takes this process out of every group the
		// pipeline could signal, which is exactly what a daemonizing worker
		// script or an agent's sandboxed tool runner does.
		if _, err := syscall.Setsid(); err != nil {
			os.Exit(4)
		}
		_ = os.WriteFile(os.Getenv("NM_PROCREAP_READY"), []byte("ready"), 0o644)
		time.Sleep(5 * time.Minute)
		os.Exit(0)
	}
}

func TestParseETime(t *testing.T) {
	cases := map[string]time.Duration{
		"01:33":      time.Minute + 33*time.Second,
		"10:01:33":   10*time.Hour + time.Minute + 33*time.Second,
		"2-10:01:33": 2*24*time.Hour + 10*time.Hour + time.Minute + 33*time.Second,
		"00:00":      0,
		"1-00:00:00": 24 * time.Hour,
		"123:04:05":  123*time.Hour + 4*time.Minute + 5*time.Second,
	}
	for value, want := range cases {
		got, err := parseETime(value)
		if err != nil {
			t.Fatalf("parseETime(%q): %v", value, err)
		}
		if got != want {
			t.Fatalf("parseETime(%q) = %v, want %v", value, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "1:2:3:4", "12"} {
		if _, err := parseETime(bad); err == nil {
			t.Fatalf("parseETime(%q) succeeded; want error", bad)
		}
	}
}

func TestParseProcessTableKeepsFullCommandLine(t *testing.T) {
	out := "  501     1   501 2-10:01:33 /bin/bash /path/to/fm-remote-job-worker.sh  --flag  value\n" +
		"bogus line\n" +
		"  502   501   502       01:02 sleep 30\n"
	procs := parseProcessTable(out)
	if len(procs) != 2 {
		t.Fatalf("parseProcessTable returned %d entries: %+v", len(procs), procs)
	}
	if procs[0].PID != 501 || procs[0].PPID != 1 || procs[0].PGID != 501 {
		t.Fatalf("unexpected first entry: %+v", procs[0])
	}
	if want := 2*24*time.Hour + 10*time.Hour + time.Minute + 33*time.Second; procs[0].Elapsed != want {
		t.Fatalf("Elapsed = %v, want %v", procs[0].Elapsed, want)
	}
	if want := "/bin/bash /path/to/fm-remote-job-worker.sh  --flag  value"; procs[0].Command != want {
		t.Fatalf("Command = %q, want %q", procs[0].Command, want)
	}
	if procs[1].PID != 502 || procs[1].Command != "sleep 30" {
		t.Fatalf("unexpected second entry: %+v", procs[1])
	}
}

func TestParseLsofCWDKeepsDeletedDirectories(t *testing.T) {
	out := "p123\nn/Users/x/.no-mistakes/worktrees/repo/run (deleted)\np124\nn/tmp\np125\n"
	cwds := parseLsofCWD(out)
	if got, want := cwds[123], "/Users/x/.no-mistakes/worktrees/repo/run"; got != want {
		t.Fatalf("cwds[123] = %q, want %q", got, want)
	}
	if got, want := cwds[124], "/tmp"; got != want {
		t.Fatalf("cwds[124] = %q, want %q", got, want)
	}
	if _, ok := cwds[125]; ok {
		t.Fatalf("pid without a name line must not appear: %v", cwds)
	}
}

func newFakeWorktree(t *testing.T) (root, worktree string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "worktrees")
	worktree = filepath.Join(root, "repo1", "run1")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	return root, worktree
}

// startEscapeeUnderLeader runs a leader in its own process group, has it spawn
// a setsid child standing in the worktree, then tears the leader's group down
// exactly the way the pipeline does. The returned pid is what survived.
func startEscapeeUnderLeader(t *testing.T, worktree string) int {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "escaped.ready")
	leader := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestProcReapHelper$")
	leader.Dir = worktree
	leader.Env = append(os.Environ(),
		"NM_PROCREAP_HELPER=leader",
		"NM_PROCREAP_READY="+ready,
		"NM_PROCREAP_WORKDIR="+worktree,
	)
	shellenv.ConfigureShellCommand(leader)
	out, err := shellenv.CombinedOutputShellCommand(leader)
	if err != nil {
		t.Fatalf("leader failed: %v; output %q", err, out)
	}
	pid := parseEscapedPID(t, string(out))
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

func parseEscapedPID(t *testing.T, output string) int {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if rest, ok := strings.CutPrefix(line, "escaped pid "); ok {
			pid, err := strconv.Atoi(strings.TrimSpace(rest))
			if err == nil && pid > 0 {
				return pid
			}
		}
	}
	t.Fatalf("helper output %q did not report an escaped pid", output)
	return 0
}

// requireCWDLookup skips when this platform cannot resolve a process working
// directory (no lsof, or a sandbox that hides it). The sweep degrades to a
// no-op there, which is correct but not what these tests assert.
func requireCWDLookup(t *testing.T) {
	t.Helper()
	self := os.Getpid()
	cwds := processCWDs([]int{self})
	if _, ok := cwds[self]; !ok {
		t.Skip("process working directories are not readable on this host")
	}
}

func parentOf(t *testing.T, pid int) int {
	t.Helper()
	procs, err := listProcesses()
	if err != nil {
		t.Fatalf("listProcesses: %v", err)
	}
	for _, p := range procs {
		if p.PID == pid {
			return p.PPID
		}
	}
	return 0
}

func containsPID(victims []Victim, pid int) bool {
	for _, v := range victims {
		if v.PID == pid {
			return true
		}
	}
	return false
}

func waitForFile(path string, timeout time.Duration) bool {
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
		if !processAlive(pid) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return !processAlive(pid)
}
