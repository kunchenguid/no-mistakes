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
	// listCalls and cwdCalls count the process-table reads, which are what a
	// sweep costs on a real machine (`ps` plus one cwd read per candidate).
	listCalls int
	cwdCalls  int
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
	listProcessesFunc = func() ([]Process, error) {
		f.mu.Lock()
		f.listCalls++
		f.mu.Unlock()
		return f.procs, nil
	}
	processCWDsFunc = func(pids []int) map[int]string {
		f.mu.Lock()
		f.cwdCalls++
		f.mu.Unlock()
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

// TestSweepRunWorktreesReapsEveryTargetFromOneProcessSnapshot covers the eject
// call, which removes every worktree a repository recorded outside the default
// tree at once. Reading the process table is what a sweep costs, and run rows
// are never pruned, so a per-directory sweep makes eject cost one `ps` plus one
// cwd read per run the repository has ever had. One snapshot must answer all of
// them, with the per-directory reach unchanged.
func TestSweepRunWorktreesReapsEveryTargetFromOneProcessSnapshot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worktrees")
	operatorRoot := filepath.Join(t.TempDir(), "repo-runs")
	first := filepath.Join(operatorRoot, "RUN1")
	second := filepath.Join(operatorRoot, "RUN2")
	third := filepath.Join(operatorRoot, "RUN3")
	fake := &fakeSystem{
		procs: []Process{
			{PID: 100, PPID: 1, PGID: 100, Command: "leaked in first", Elapsed: time.Second},
			{PID: 200, PPID: 1, PGID: 200, Command: "leaked in second", Elapsed: 40 * time.Hour},
			{PID: 300, PPID: 1, PGID: 300, Command: "leaked below third", Elapsed: time.Second},
			{PID: 400, PPID: 1, PGID: 400, Command: "operator's own work", Elapsed: 40 * time.Hour},
			{PID: 500, PPID: 1, PGID: 500, Command: "unswept run of another repo", Elapsed: 40 * time.Hour},
		},
		cwds: map[int]string{
			100: first,
			200: second,
			300: filepath.Join(third, "internal", "gate"),
			400: filepath.Join(operatorRoot, "scratch-checkout"),
			500: filepath.Join(operatorRoot, "RUN9"),
		},
	}
	fake.install(t)

	SweepRunWorktrees(root, []Worktree{
		{Dir: first, RepoID: "repo1", RunID: "RUN1"},
		{Dir: second, RepoID: "repo1", RunID: "RUN2"},
		{Dir: third, RepoID: "repo1", RunID: "RUN3"},
	}, "eject")

	for _, pid := range []int{100, 200, 300} {
		if !fake.anySignalTo(pid) {
			t.Fatalf("process %d standing in a swept worktree survived: %+v", pid, fake.signals)
		}
	}
	for _, pid := range []int{400, 500} {
		if fake.anySignalTo(pid) {
			t.Fatalf("sweep reached %d, which stands in a directory no named worktree covers: %+v", pid, fake.signals)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.listCalls != 1 || fake.cwdCalls != 1 {
		t.Fatalf("process table read %d times and cwds %d times for 3 worktrees, want 1 each", fake.listCalls, fake.cwdCalls)
	}
}

// TestSweepRunWorktreesWithoutTargetsReadsNothing pins the fail-safe half of the
// batched entry point: with no worktree to sweep there is nothing to scope the
// sweep to, and an unscoped sweep of the default tree with no age floor and no
// run-active check would reap a live run's processes.
func TestSweepRunWorktreesWithoutTargetsReadsNothing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worktrees")
	fake := &fakeSystem{
		procs: []Process{{PID: 100, PPID: 1, PGID: 100, Command: "running step", Elapsed: 40 * time.Hour}},
		cwds:  map[int]string{100: filepath.Join(root, "repo1", "run1")},
	}
	fake.install(t)

	SweepRunWorktrees(root, nil, "eject")
	SweepRunWorktrees(root, []Worktree{{Dir: "  ", RepoID: "repo1", RunID: "run1"}}, "eject")

	if fake.anySignalTo(100) {
		t.Fatalf("a sweep with no target signalled a process in the default tree: %+v", fake.signals)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.listCalls != 0 {
		t.Fatalf("process table read %d times with no worktree to sweep, want 0", fake.listCalls)
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

	victims, err := Sweep(Options{WorktreesRoot: root, Scopes: []string{scope}})
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

	victims, err := Sweep(Options{WorktreesRoot: root, Scopes: []string{wt}})
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

	victims, err := Sweep(Options{WorktreesRoot: root, Scopes: []string{wt}})
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

	if _, err := Sweep(Options{WorktreesRoot: root, Scopes: []string{wt}, Grace: 100 * time.Millisecond}); err != nil {
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
	roots := worktreeMatchers(Options{WorktreesRoot: root})
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

// TestSweepNamedWorktreesReachOnlyWhatARunRecorded covers a repository whose
// runs the operator placed in a directory of their own (worktree_roots). Reach
// there is the worktrees the caller names, one per run record, so a stale run
// process is still reaped - while everything else in that directory is out of
// reach, including a directory whose name looks exactly like a run worktree but
// that no run created. The name is not evidence in somebody else's directory.
func TestSweepNamedWorktreesReachOnlyWhatARunRecorded(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worktrees")
	repoRoot := filepath.Join(t.TempDir(), "repo-runs")
	const doneRun = "01JZ8XQ7V6K9M3B0T5N2R4C8YD"
	const activeRun = "01JZ8XQ7V6K9M3B0T5N2R4C8YE"
	const unclaimedRun = "01JZ8XQ7V6K9M3B0T5N2R4C8YF"
	fake := &fakeSystem{
		procs: []Process{
			{PID: 100, PPID: 1, PGID: 100, Command: "leaked", Elapsed: 40 * time.Hour},
			{PID: 200, PPID: 1, PGID: 200, Command: "operator editor", Elapsed: 40 * time.Hour},
			{PID: 300, PPID: 1, PGID: 300, Command: "live run", Elapsed: 40 * time.Hour},
			{PID: 400, PPID: 1, PGID: 400, Command: "operator worker", Elapsed: 40 * time.Hour},
		},
		cwds: map[int]string{
			100: filepath.Join(repoRoot, doneRun, "internal"),
			200: filepath.Join(repoRoot, "scratch-checkout"),
			300: filepath.Join(repoRoot, activeRun),
			400: filepath.Join(repoRoot, unclaimedRun, "internal"),
		},
	}
	fake.install(t)

	victims, err := Sweep(Options{
		WorktreesRoot: root,
		Worktrees: []Worktree{
			{Dir: filepath.Join(repoRoot, doneRun), RepoID: "repo1", RunID: doneRun},
			{Dir: filepath.Join(repoRoot, activeRun), RepoID: "repo1", RunID: activeRun},
		},
		MinAge:    DefaultMinAge,
		RunActive: func(repoID, runID string) bool { return repoID == "repo1" && runID == activeRun },
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got := victimPIDs(victims); len(got) != 1 || got[0] != 100 {
		t.Fatalf("Sweep victims = %v, want [100]", got)
	}
	if fake.anySignalTo(200) {
		t.Fatalf("a process in the operator's own directory must never be signalled: %+v", fake.signals)
	}
	if fake.anySignalTo(300) {
		t.Fatalf("active run's process must never be signalled: %+v", fake.signals)
	}
	if fake.anySignalTo(400) {
		t.Fatalf("a run-shaped directory no run recorded must never be signalled: %+v", fake.signals)
	}
}

// TestSweepReachesDeletedNamedWorktreeThroughSymlinkedRoot is the deleted-cwd
// case this package exists for, in an operator's own directory. The worktree is
// gone - the daemon removed it at run end while a process that had left its
// process group kept standing in it - so the only spelling left to resolve is
// the directory that held it. On macOS every path under /tmp goes through a
// symlink, so the recorded spelling and the process's cwd differ, and matching
// only the recorded one leaves that process burning CPU forever.
func TestSweepReachesDeletedNamedWorktreeThroughSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real-runs")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(base, "runs")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	const runID = "01JZ8XQ7V6K9M3B0T5N2R4C8YD"
	// /proc and lsof report a cwd with every symlink already resolved, which is
	// the spelling that survives the directory's removal.
	resolvedRoot, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeSystem{
		procs: []Process{{PID: 100, PPID: 1, PGID: 100, Command: "leaked", Elapsed: 40 * time.Hour}},
		cwds:  map[int]string{100: filepath.Join(resolvedRoot, runID, "internal")},
	}
	fake.install(t)

	victims, sweepErr := Sweep(Options{
		WorktreesRoot: filepath.Join(base, "worktrees"),
		// Recorded the way the run created it, through the symlink, and long
		// since removed from disk.
		Worktrees: []Worktree{{Dir: filepath.Join(linkedRoot, runID), RepoID: "repo1", RunID: runID}},
		MinAge:    DefaultMinAge,
		RunActive: func(string, string) bool { return false },
	})
	if sweepErr != nil {
		t.Fatalf("Sweep: %v", sweepErr)
	}
	if got := victimPIDs(victims); len(got) != 1 || got[0] != 100 {
		t.Fatalf("Sweep victims = %v, want [100]: a deleted worktree recorded through a symlink is still the process's own worktree", got)
	}
}

// A named worktree without the run identity to check it against is dropped
// rather than matched, so a caller that cannot say which run owns a directory
// never gets it signalled.
func TestSweepIgnoresNamedWorktreeWithoutRunIdentity(t *testing.T) {
	repoRoot := filepath.Join(t.TempDir(), "repo-runs")
	const runID = "01JZ8XQ7V6K9M3B0T5N2R4C8YD"
	fake := &fakeSystem{
		procs: []Process{{PID: 100, PPID: 1, PGID: 100, Command: "leaked", Elapsed: 40 * time.Hour}},
		cwds:  map[int]string{100: filepath.Join(repoRoot, runID)},
	}
	fake.install(t)

	victims, err := Sweep(Options{
		WorktreesRoot: filepath.Join(t.TempDir(), "worktrees"),
		Worktrees:     []Worktree{{Dir: filepath.Join(repoRoot, runID)}},
		MinAge:        DefaultMinAge,
		RunActive:     func(string, string) bool { return false },
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(victims) != 0 || fake.anySignalTo(100) {
		t.Fatalf("worktree without a run identity was swept: victims=%v signals=%+v", victimPIDs(victims), fake.signals)
	}
}
