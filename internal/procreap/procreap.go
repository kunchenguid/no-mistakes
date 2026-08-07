// Package procreap finds and terminates processes that outlived the pipeline
// run whose worktree they were launched in.
//
// Why a sweep is needed at all. Every subprocess no-mistakes spawns is already
// isolated in its own process group by internal/shellenv, and that group is
// killed on cancellation and on every exit path. A process group is, however,
// only a *lineage* container: a descendant that calls setsid(2) or setpgid(2)
// - which agent CLIs do for their own tool sandboxing, and which any
// daemonizing helper script does on purpose - leaves the group and becomes
// invisible to kill(-pgid). Once its parent dies it reparents to init, and
// from that moment no lineage-based mechanism (process group, parent chain,
// exec.Cmd handle) can ever name it again. Such a process keeps burning CPU
// and holding a deleted worktree's cwd open indefinitely.
//
// The one identity that survives lineage loss is where the process is
// standing: its current working directory. Pipeline children are launched in
// (or below) the run's worktree, so a live process whose cwd resolves under
// <NM_HOME>/worktrees/<repoID>/<runID> belongs to that run, no matter who its
// parent is now. That is the association this package matches on.
//
// Deliberately NOT matched: the command line. A path can appear in argv for
// entirely legitimate reasons (the daemon's own `git worktree remove`, a
// grep, an editor), so argv matching trades a precise signal for a
// false-positive that would kill an innocent process. cwd is held by exactly
// the processes that are actually running inside the run.
//
// Safety rules, in order:
//   - pid 0 and 1 are never signalled, and neither is this process or any of
//     its ancestors.
//   - A worktree whose run is still pending or running is never swept
//     (Options.RunActive).
//   - Unscoped sweeps additionally require Options.MinAge, so a process that
//     started moments ago is never mistaken for a leak.
//   - SIGTERM first; SIGKILL only for what is still alive after Options.Grace.
//
// A process whose cwd is outside <NM_HOME>/worktrees can never match, which is
// what keeps long-lived unrelated daemons (a user's LaunchAgent worker, an
// editor, another tool's background job) out of reach by construction rather
// than by name-based allowlisting.
package procreap

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultMinAge is the age floor for an unscoped sweep. It is comfortably
	// longer than any single pipeline step's startup so a process that belongs
	// to a run the daemon is about to resume is never a candidate.
	DefaultMinAge = 10 * time.Minute

	// DefaultGrace is how long a matched process may take to exit after
	// SIGTERM before it is SIGKILLed.
	DefaultGrace = 3 * time.Second
)

// Process is one entry of the system process table.
type Process struct {
	PID     int
	PPID    int
	PGID    int
	Command string
	Elapsed time.Duration
}

// Victim is a process the sweep terminated, reported for logging.
type Victim struct {
	PID      int
	PGID     int
	Command  string
	Worktree string
	Killed   bool // true when SIGTERM was not enough and SIGKILL was needed
}

// Options configures a sweep.
type Options struct {
	// WorktreesRoot is <NM_HOME>/worktrees. Required.
	WorktreesRoot string

	// Scope, when set, restricts the sweep to exactly this worktree directory
	// and to its subdirectories. The caller that sets it owns that run and has
	// already observed its execution return, so RunActive and MinAge are not
	// consulted for a scoped sweep.
	Scope string

	// MinAge skips processes younger than this. Ignored when Scope is set.
	MinAge time.Duration

	// RunActive reports whether the run still owns its worktree; its worktree
	// is then left alone. A nil RunActive means "no run is active", which is
	// only correct for a scoped sweep. Ignored when Scope is set.
	RunActive func(repoID, runID string) bool

	// Grace is how long a signalled process may exit before SIGKILL.
	// Zero means DefaultGrace.
	Grace time.Duration
}

// seams, overridden in tests.
var (
	listProcessesFunc = listProcesses
	processCWDsFunc   = processCWDs
	signalProcessFunc = signalProcess
	signalGroupFunc   = signalGroup
	processAliveFunc  = processAlive
)

// Sweep terminates every process associated with a worktree that no run owns
// any more. It returns the processes it signalled. An empty result with a nil
// error is the normal outcome and also what platforms without a process-table
// reader (Windows, where job objects already contain the whole tree) return.
func Sweep(opts Options) ([]Victim, error) {
	if strings.TrimSpace(opts.WorktreesRoot) == "" {
		return nil, nil
	}
	procs, err := listProcessesFunc()
	if err != nil {
		return nil, err
	}
	if len(procs) == 0 {
		return nil, nil
	}

	byPID := make(map[int]Process, len(procs))
	for _, p := range procs {
		byPID[p.PID] = p
	}
	protected := protectedPIDs(byPID)

	candidates := make([]int, 0, len(procs))
	for _, p := range procs {
		if p.PID <= 1 || protected[p.PID] {
			continue
		}
		if opts.Scope == "" && opts.MinAge > 0 && p.Elapsed < opts.MinAge {
			continue
		}
		candidates = append(candidates, p.PID)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	cwds := processCWDsFunc(candidates)
	roots := pathPrefixes(opts.WorktreesRoot)
	scopes := pathPrefixes(opts.Scope)

	matched := make(map[int]string)
	for pid, cwd := range cwds {
		dir, repoID, runID, ok := worktreeRef(roots, cwd)
		if !ok {
			continue
		}
		if len(scopes) > 0 {
			if !underAny(scopes, cwd) {
				continue
			}
		} else if opts.RunActive != nil && opts.RunActive(repoID, runID) {
			continue
		}
		matched[pid] = dir
	}
	if len(matched) == 0 {
		return nil, nil
	}

	victims := expandVictims(matched, procs, protected)
	terminate(victims, opts.Grace)
	return victims, nil
}

// SweepAndLog runs Sweep and reports the outcome on the daemon log. It is the
// form both call sites want: a sweep is best effort, and a failure to read the
// process table must never fail a run or block daemon startup.
func SweepAndLog(opts Options, reason string) {
	victims, err := Sweep(opts)
	if err != nil {
		slog.Warn("orphan process sweep failed", "reason", reason, "error", err)
		return
	}
	for _, v := range victims {
		slog.Info("reaped orphaned run process",
			"reason", reason,
			"pid", v.PID,
			"worktree", v.Worktree,
			"command", v.Command,
			"sigkill", v.Killed)
	}
}

// protectedPIDs is this process plus its ancestor chain. Signalling any of
// them would take down the daemon (or whatever launched it) while trying to
// clean up after it.
func protectedPIDs(byPID map[int]Process) map[int]bool {
	protected := map[int]bool{os.Getpid(): true}
	pid := os.Getpid()
	for range len(byPID) + 1 {
		p, ok := byPID[pid]
		if !ok || p.PPID <= 1 || protected[p.PPID] {
			break
		}
		protected[p.PPID] = true
		pid = p.PPID
	}
	return protected
}

// pathPrefixes returns the comparable forms of dir: the cleaned path and, when
// it differs, its symlink-resolved form. macOS reports /private/var for a
// /var path, so a single spelling is not enough to recognise our own tree.
func pathPrefixes(dir string) []string {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	cleaned := filepath.Clean(dir)
	out := []string{cleaned}
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil && resolved != cleaned {
		out = append(out, resolved)
	}
	return out
}

// worktreeRef reports the run worktree that path sits in. It requires at least
// <root>/<repoID>/<runID>, so a process standing in the worktrees root itself,
// or in a repo directory with no run below it, never matches.
func worktreeRef(roots []string, path string) (dir, repoID, runID string, ok bool) {
	if path == "" {
		return "", "", "", false
	}
	cleaned := filepath.Clean(path)
	for _, root := range roots {
		rel, err := filepath.Rel(root, cleaned)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			continue
		}
		return filepath.Join(root, parts[0], parts[1]), parts[0], parts[1], true
	}
	return "", "", "", false
}

func underAny(prefixes []string, path string) bool {
	cleaned := filepath.Clean(path)
	for _, prefix := range prefixes {
		rel, err := filepath.Rel(prefix, cleaned)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		return true
	}
	return false
}

// expandVictims grows the matched set to everything that must die with it:
// the transitive children of a matched process, and the members of a process
// group a matched process leads. The group expansion is deliberately limited
// to groups a victim *leads* (pgid == victim pid); expanding into an inherited
// group could reach processes that merely share a session with us.
func expandVictims(matched map[int]string, procs []Process, protected map[int]bool) []Victim {
	selected := make(map[int]string, len(matched))
	for pid, dir := range matched {
		selected[pid] = dir
	}

	children := make(map[int][]Process, len(procs))
	byGroup := make(map[int][]Process, len(procs))
	for _, p := range procs {
		children[p.PPID] = append(children[p.PPID], p)
		byGroup[p.PGID] = append(byGroup[p.PGID], p)
	}

	queue := make([]int, 0, len(matched))
	for pid := range matched {
		queue = append(queue, pid)
	}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		dir := selected[pid]
		related := append(append([]Process(nil), children[pid]...), byGroup[pid]...)
		for _, p := range related {
			if p.PID <= 1 || protected[p.PID] || p.PID == pid {
				continue
			}
			if _, seen := selected[p.PID]; seen {
				continue
			}
			selected[p.PID] = dir
			queue = append(queue, p.PID)
		}
	}

	info := make(map[int]Process, len(procs))
	for _, p := range procs {
		info[p.PID] = p
	}
	victims := make([]Victim, 0, len(selected))
	for pid, dir := range selected {
		p := info[pid]
		victims = append(victims, Victim{PID: pid, PGID: p.PGID, Command: p.Command, Worktree: dir})
	}
	sort.Slice(victims, func(i, j int) bool { return victims[i].PID < victims[j].PID })
	return victims
}

// terminate asks every victim to exit, then kills whatever is still alive
// after grace. SIGTERM first is not politeness: a test runner or a worker
// script given the chance to exit flushes its output and removes its own
// temporary state, which SIGKILL denies it.
func terminate(victims []Victim, grace time.Duration) {
	if grace <= 0 {
		grace = DefaultGrace
	}
	for _, v := range victims {
		signalVictim(v, sigTerm)
	}
	deadline := time.Now().Add(grace)
	for {
		remaining := false
		for _, v := range victims {
			if processAliveFunc(v.PID) {
				remaining = true
				break
			}
		}
		if !remaining || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for i := range victims {
		if !processAliveFunc(victims[i].PID) {
			continue
		}
		victims[i].Killed = true
		signalVictim(victims[i], sigKill)
	}
}

// signalVictim signals the process and, when it leads its own group, the group
// as well, so a child it spawned between listing and signalling is covered.
// The group signal is limited to a group the victim leads: a process group id
// is the pid of its (possibly already dead) leader, so kill(-pid) for a
// non-leader could land on a leftover group that reused this pid number.
func signalVictim(v Victim, sig procSignal) {
	_ = signalProcessFunc(v.PID, sig)
	if v.PGID == v.PID && v.PGID > 1 {
		_ = signalGroupFunc(v.PGID, sig)
	}
}
