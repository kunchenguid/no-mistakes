package procreap

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeSystem is an in-memory process table with recorded signals, so the
// sweep's policy can be exercised without signalling anything real.
type fakeSystem struct {
	mu      sync.Mutex
	procs   []Process
	cwds    map[int]string
	dead    map[int]bool
	signals []signalRecord
	// ignoreTerm names processes that keep running after SIGTERM, so the
	// SIGKILL escalation can be observed.
	ignoreTerm map[int]bool
}

type signalRecord struct {
	pid   int
	group bool
	sig   procSignal
}

func (f *fakeSystem) install(t *testing.T) {
	t.Helper()
	if f.dead == nil {
		f.dead = map[int]bool{}
	}
	if f.ignoreTerm == nil {
		f.ignoreTerm = map[int]bool{}
	}
	origList, origCWD, origSignal, origGroup, origAlive := listProcessesFunc, processCWDsFunc, signalProcessFunc, signalGroupFunc, processAliveFunc
	t.Cleanup(func() {
		listProcessesFunc, processCWDsFunc, signalProcessFunc, signalGroupFunc, processAliveFunc = origList, origCWD, origSignal, origGroup, origAlive
	})
	listProcessesFunc = func() ([]Process, error) { return f.procs, nil }
	processCWDsFunc = func(pids []int) map[int]string {
		out := make(map[int]string, len(pids))
		for _, pid := range pids {
			if cwd, ok := f.cwds[pid]; ok {
				out[pid] = cwd
			}
		}
		return out
	}
	signalProcessFunc = func(pid int, sig procSignal) error {
		f.record(pid, false, sig)
		return nil
	}
	signalGroupFunc = func(pgid int, sig procSignal) error {
		f.record(pgid, true, sig)
		return nil
	}
	processAliveFunc = func(pid int) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return !f.dead[pid]
	}
}

func (f *fakeSystem) record(pid int, group bool, sig procSignal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, signalRecord{pid: pid, group: group, sig: sig})
	if sig == sigTerm && !f.ignoreTerm[pid] {
		f.dead[pid] = true
	}
	if sig == sigKill {
		f.dead[pid] = true
	}
}

func (f *fakeSystem) sentTo(pid int, sig procSignal) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.signals {
		if s.pid == pid && s.sig == sig {
			return true
		}
	}
	return false
}

func (f *fakeSystem) anySignalTo(pid int) bool {
	return f.sentTo(pid, sigTerm) || f.sentTo(pid, sigKill)
}

func victimPIDs(victims []Victim) []int {
	out := make([]int, 0, len(victims))
	for _, v := range victims {
		out = append(out, v.PID)
	}
	return out
}

// TestSweepReapsStaleWorktreeProcessAndSparesEverythingElse is the core policy
// contract: a long-running process standing in a finished run's worktree is
// terminated, while a process of the same age standing anywhere else - the
// case that protects an unrelated LaunchAgent-style worker - is untouched.
func TestSweepReapsStaleWorktreeProcessAndSparesEverythingElse(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worktrees")
	fake := &fakeSystem{
		procs: []Process{
			{PID: 100, PPID: 1, PGID: 100, Command: "fm-remote-job-worker.sh", Elapsed: 40 * time.Hour},
			{PID: 200, PPID: 1, PGID: 200, Command: "unrelated-worker", Elapsed: 40 * time.Hour},
		},
		cwds: map[int]string{
			100: filepath.Join(root, "repo1", "run1", "bin"),
			200: filepath.Join(t.TempDir(), "elsewhere"),
		},
	}
	fake.install(t)

	victims, err := Sweep(Options{WorktreesRoot: root, MinAge: DefaultMinAge})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got := victimPIDs(victims); len(got) != 1 || got[0] != 100 {
		t.Fatalf("Sweep victims = %v, want [100]", got)
	}
	if !fake.sentTo(100, sigTerm) {
		t.Fatalf("stale worktree process 100 was never signalled: %+v", fake.signals)
	}
	if fake.anySignalTo(200) {
		t.Fatalf("process outside the worktrees root must never be signalled: %+v", fake.signals)
	}
}

// TestSweepSparesActiveRunWorktree pins the guard that keeps the reaper from
// killing a live pipeline: an active run still owns its worktree, so nothing
// standing in it is a leak, no matter how long it has been running.
func TestSweepSparesActiveRunWorktree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worktrees")
	fake := &fakeSystem{
		procs: []Process{
			{PID: 100, PPID: 1, PGID: 100, Command: "go test ./...", Elapsed: 40 * time.Hour},
			{PID: 101, PPID: 1, PGID: 101, Command: "go test ./...", Elapsed: 40 * time.Hour},
		},
		cwds: map[int]string{
			100: filepath.Join(root, "repo1", "activeRun"),
			101: filepath.Join(root, "repo1", "doneRun"),
		},
	}
	fake.install(t)

	victims, err := Sweep(Options{
		WorktreesRoot: root,
		MinAge:        DefaultMinAge,
		RunActive:     func(_, runID string) bool { return runID == "activeRun" },
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got := victimPIDs(victims); len(got) != 1 || got[0] != 101 {
		t.Fatalf("Sweep victims = %v, want [101]", got)
	}
	if fake.anySignalTo(100) {
		t.Fatalf("active run's process must never be signalled: %+v", fake.signals)
	}
}

// TestSweepMinAgeSparesYoungProcesses guards the startup sweep against
// terminating a process that just started, which is what a run beginning
// concurrently with daemon startup would look like.
func TestSweepMinAgeSparesYoungProcesses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worktrees")
	fake := &fakeSystem{
		procs: []Process{{PID: 100, PPID: 1, PGID: 100, Command: "young", Elapsed: 30 * time.Second}},
		cwds:  map[int]string{100: filepath.Join(root, "repo1", "run1")},
	}
	fake.install(t)

	victims, err := Sweep(Options{WorktreesRoot: root, MinAge: DefaultMinAge})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(victims) != 0 {
		t.Fatalf("Sweep victims = %v, want none", victimPIDs(victims))
	}
}

// TestSweepScopeIgnoresAgeAndOtherWorktrees covers the run-cleanup call: the
// caller owns exactly one finished run, so its worktree is swept regardless of
// how young the processes are, and no other run's worktree is touched.
func TestSweepScopeIgnoresAgeAndOtherWorktrees(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worktrees")
	scope := filepath.Join(root, "repo1", "run1")
	fake := &fakeSystem{
		procs: []Process{
			{PID: 100, PPID: 1, PGID: 100, Command: "in scope", Elapsed: time.Second},
			{PID: 200, PPID: 1, PGID: 200, Command: "other run", Elapsed: 40 * time.Hour},
		},
		cwds: map[int]string{
			100: filepath.Join(scope, "internal", "branchsync"),
			200: filepath.Join(root, "repo1", "run2"),
		},
	}
	fake.install(t)

	victims, err := Sweep(Options{WorktreesRoot: root, Scope: scope})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got := victimPIDs(victims); len(got) != 1 || got[0] != 100 {
		t.Fatalf("Sweep victims = %v, want [100]", got)
	}
	if fake.anySignalTo(200) {
		t.Fatalf("a scoped sweep must not reach another run's worktree: %+v", fake.signals)
	}
}

// TestSweepNeverSignalsItselfOrItsAncestors is the guard that keeps a sweep
// from taking down the daemon that is running it - the daemon's own working
// directory is not normally a worktree, but a fixture, a test, or a future
// caller must not be able to make it one and lose the process.
func TestSweepNeverSignalsItselfOrItsAncestors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worktrees")
	self := os.Getpid()
	parent := os.Getppid()
	if parent <= 1 || parent == self {
		t.Skip("test process has no distinguishable parent to protect")
	}
	wt := filepath.Join(root, "repo1", "run1")
	fake := &fakeSystem{
		procs: []Process{
			{PID: self, PPID: parent, PGID: self, Command: "no-mistakes daemon run", Elapsed: time.Hour},
			{PID: parent, PPID: 1, PGID: parent, Command: "launcher", Elapsed: time.Hour},
			{PID: 999, PPID: 1, PGID: 999, Command: "leaked", Elapsed: time.Hour},
		},
		cwds: map[int]string{self: wt, parent: wt, 999: wt},
	}
	fake.install(t)

	victims, err := Sweep(Options{WorktreesRoot: root, Scope: wt})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got := victimPIDs(victims); len(got) != 1 || got[0] != 999 {
		t.Fatalf("Sweep victims = %v, want [999]", got)
	}
	if fake.anySignalTo(self) || fake.anySignalTo(parent) {
		t.Fatalf("sweep signalled itself or an ancestor: %+v", fake.signals)
	}
}

// TestSweepExpandsToDescendantsAndLedGroup pins that a matched process takes
// its whole tree with it: a child that has since chdir'd out of the worktree,
// and a sibling sharing the group it leads, are both reaped. Without this a
// leaked worker's own children keep running after the worker is gone.
func TestSweepExpandsToDescendantsAndLedGroup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worktrees")
	wt := filepath.Join(root, "repo1", "run1")
	fake := &fakeSystem{
		procs: []Process{
			{PID: 100, PPID: 1, PGID: 100, Command: "leaked leader", Elapsed: time.Hour},
			{PID: 101, PPID: 100, PGID: 555, Command: "child that moved away", Elapsed: time.Hour},
			{PID: 102, PPID: 999, PGID: 100, Command: "group member", Elapsed: time.Hour},
			{PID: 103, PPID: 101, PGID: 555, Command: "grandchild", Elapsed: time.Hour},
			{PID: 300, PPID: 1, PGID: 300, Command: "stranger", Elapsed: time.Hour},
		},
		cwds: map[int]string{100: wt, 101: "/tmp", 102: "/tmp", 103: "/tmp", 300: "/tmp"},
	}
	fake.install(t)

	victims, err := Sweep(Options{WorktreesRoot: root, Scope: wt})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	got := map[int]bool{}
	for _, pid := range victimPIDs(victims) {
		got[pid] = true
	}
	for _, want := range []int{100, 101, 102, 103} {
		if !got[want] {
			t.Fatalf("Sweep victims = %v, want it to include %d", victimPIDs(victims), want)
		}
	}
	if got[300] {
		t.Fatalf("Sweep reached an unrelated process: %v", victimPIDs(victims))
	}
}

// TestSweepEscalatesToSIGKILLOnlyAfterSIGTERMFails pins the termination
// contract: everything gets a chance to shut down cleanly, and only what
// survives the grace period is killed outright.
func TestSweepEscalatesToSIGKILLOnlyAfterSIGTERMFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worktrees")
	wt := filepath.Join(root, "repo1", "run1")
	fake := &fakeSystem{
		procs: []Process{
			{PID: 100, PPID: 1, PGID: 100, Command: "polite", Elapsed: time.Hour},
			{PID: 200, PPID: 1, PGID: 200, Command: "stubborn", Elapsed: time.Hour},
		},
		cwds:       map[int]string{100: wt, 200: wt},
		ignoreTerm: map[int]bool{200: true},
	}
	fake.install(t)

	if _, err := Sweep(Options{WorktreesRoot: root, Scope: wt, Grace: 100 * time.Millisecond}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !fake.sentTo(100, sigTerm) {
		t.Fatalf("polite process was never asked to exit: %+v", fake.signals)
	}
	if fake.sentTo(100, sigKill) {
		t.Fatalf("a process that exited on SIGTERM must not be SIGKILLed: %+v", fake.signals)
	}
	if !fake.sentTo(200, sigKill) {
		t.Fatalf("a process that ignored SIGTERM must be SIGKILLed: %+v", fake.signals)
	}
}

// TestWorktreeRefRequiresRunDirectory keeps the association precise: standing
// in the worktrees root or in a repo directory is not standing in a run, and
// must never make a process a candidate.
func TestWorktreeRefRequiresRunDirectory(t *testing.T) {
	root := t.TempDir()
	roots := pathPrefixes(root)
	for _, path := range []string{root, filepath.Join(root, "repo1"), filepath.Dir(root), "/tmp"} {
		if _, _, _, ok := worktreeRef(roots, path); ok {
			t.Fatalf("worktreeRef(%q) matched; want no match", path)
		}
	}
	dir, repoID, runID, ok := worktreeRef(roots, filepath.Join(root, "repo1", "run1", "internal", "git"))
	if !ok || repoID != "repo1" || runID != "run1" || dir != filepath.Join(root, "repo1", "run1") {
		t.Fatalf("worktreeRef = (%q, %q, %q, %v)", dir, repoID, runID, ok)
	}
}
