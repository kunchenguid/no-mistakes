package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// pi_liveness.go binds one launched pi process to its exact adapter-native
// session JSONL so the pipeline's silence watchdog can credit real session
// advancement while pi's own stdout is buffered (pi in --mode json emits on a
// pipe that the wrapper may not drain for long stretches; the session file is
// the authoritative per-event record, as incident forensics proved when a
// healthy fixer was killed as "silent 30m" 23s after its last session event).
//
// Binding is deliberately narrow and fail-closed:
//   - --no-session invocations persist nothing, so there is no file to watch;
//     stdout/lifecycle evidence alone governs them.
//   - A resumed session (--session <uuid>) binds only to the one existing
//     file in the launched cwd's session dir whose name ends _<uuid>.jsonl.
//   - A fresh durable session binds only when exactly one new session file
//     appears in that dir after process start, and it unbinds if pi's
//     JSON-mode session header later names a different session id.
//   - Anything ambiguous (multiple matches, --session-dir or
//     PI_CODING_AGENT_SESSION_DIR relocating storage, an unresolvable agent
//     dir) disables the watcher: the invocation keeps the conservative
//     stdout/lifecycle behavior instead of crediting unrelated activity.
//
// The watcher polls exactly one directory listing or one file stat per tick,
// never reads session content, and never credits broad filesystem changes.

// piSessionWatchPollInterval is the watcher's tick. It is a package-level var
// so tests can shorten it, mirroring transientBackoff.
var piSessionWatchPollInterval = 2 * time.Second

// piSessionPathReplacer mirrors pi's session-dir encoding (verified against
// pi's dist/core/session-manager.js getDefaultSessionDirPath): strip one
// leading path separator, then replace every '/', '\', and ':' with '-'.
var piSessionPathReplacer = strings.NewReplacer("/", "-", "\\", "-", ":", "-")

// piSessionDirForCWD computes the directory pi persists this cwd's sessions
// to: <agentDir>/sessions/--<encoded cwd>--. Pi encodes process.cwd(), which
// the kernel resolves to the physical path, so symlinked spellings (macOS
// /var -> /private/var, symlinked homes) must be resolved here too; when
// resolution fails the launch spelling is used and binding simply stays
// unavailable (fail-closed to stdout/lifecycle liveness).
func piSessionDirForCWD(agentDir, cwd string) string {
	abs, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		abs, err = filepath.Abs(cwd)
		if err != nil {
			abs = cwd
		}
	}
	return filepath.Join(agentDir, "sessions", piEncodeSessionDirName(abs))
}

// piEncodeSessionDirName is the pure cwd→directory-name encoding, kept
// separate from path resolution so it stays testable on every platform.
func piEncodeSessionDirName(abs string) string {
	enc := abs
	if len(enc) > 0 && (enc[0] == '/' || enc[0] == '\\') {
		enc = enc[1:]
	}
	return "--" + piSessionPathReplacer.Replace(enc) + "--"
}

// piAgentDirFromEnv resolves pi's agent config dir the way pi does
// (dist/config.js getAgentDir): PI_CODING_AGENT_DIR when set, else
// <home>/.pi/agent. Home is read from the child environment the adapter
// actually launches with, falling back to the process home.
func piAgentDirFromEnv(env []string) string {
	if dir := envValue(env, "PI_CODING_AGENT_DIR"); dir != "" {
		return dir
	}
	home := envValue(env, "HOME")
	if home == "" {
		home = envValue(env, "USERPROFILE")
	}
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".pi", "agent")
}

func envValue(env []string, key string) string {
	prefix := key + "="
	value := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			value = entry[len(prefix):]
		}
	}
	return value
}

// piSessionFileID extracts the session UUID from pi's session file name
// shape <fileTimestamp>_<uuid>.jsonl, rejecting any other shape.
func piSessionFileID(name string) string {
	const suffix = ".jsonl"
	if !strings.HasSuffix(name, suffix) {
		return ""
	}
	stem := strings.TrimSuffix(name, suffix)
	// Shape: <fileTimestamp>_<uuid> - pi's timestamp prefix is never empty.
	if len(stem) < 38 || stem[len(stem)-37] != '_' {
		return ""
	}
	id := stem[len(stem)-36:]
	if !isPiSessionID(id) {
		return ""
	}
	return id
}

// piSessionFileState is the advancement fingerprint of one session file.
type piSessionFileState struct {
	size int64
	mod  time.Time
}

// piSessionWatcher watches at most one bound session file and reports its
// advancement as ActivitySession. Create it with startPiSessionWatcher; a nil
// watcher means binding was impossible and the invocation keeps conservative
// stdout/lifecycle liveness.
type piSessionWatcher struct {
	dir        string
	resumeID   string
	sessionID  func() string // late-arriving JSON-mode header id, "" until parsed
	onActivity func(ActivityKind)
	onNote     func(string)
	baseline   map[string]piSessionFileState // session files present at launch, set once before run

	stopCh chan struct{}
	doneCh chan struct{}
	stop   sync.Once
}

// startPiSessionWatcher resolves the binding policy for this invocation and,
// when a binding is possible, starts the watch goroutine. It returns nil when
// the invocation cannot have a watchable session (session-free, relocated
// session storage, or an unresolvable agent dir).
func startPiSessionWatcher(ctx context.Context, extraArgs, childEnv []string, cwd string, session *SessionRef, sessionID func() string, onActivity func(ActivityKind), onNote func(string)) *piSessionWatcher {
	if session == nil || onActivity == nil {
		// --no-session persists no file; nothing authoritative to watch.
		return nil
	}
	if envValue(childEnv, "PI_CODING_AGENT_SESSION_DIR") != "" || piArgsHaveSessionDir(extraArgs) {
		// Session storage is relocated to a flat operator-chosen directory we
		// cannot cwd-scope; binding there could credit an unrelated session.
		if onNote != nil {
			onNote("pi liveness: session storage relocated by --session-dir/PI_CODING_AGENT_SESSION_DIR; stdout-only liveness")
		}
		return nil
	}
	agentDir := piAgentDirFromEnv(childEnv)
	if agentDir == "" {
		return nil
	}
	w := &piSessionWatcher{
		dir:        piSessionDirForCWD(agentDir, cwd),
		sessionID:  sessionID,
		onActivity: onActivity,
		onNote:     onNote,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
	if session.ID != "" {
		w.resumeID = session.ID
	}
	// Snapshot pre-existing session files synchronously: a fresh durable
	// session may bind only to a file created after this point, and taking
	// the baseline inside the goroutine would race the file pi creates at
	// process start. The snapshot carries size/mtime so a resumed session
	// that already advanced between launch and the first poll is credited.
	w.baseline = w.snapshotSessionFiles()
	go w.run(ctx)
	return w
}

func piArgsHaveSessionDir(args []string) bool {
	for _, arg := range args {
		if arg == "--session-dir" || strings.HasPrefix(arg, "--session-dir=") {
			return true
		}
	}
	return false
}

// shutdown stops the watch goroutine and waits for it to exit.
func (w *piSessionWatcher) shutdown() {
	w.stop.Do(func() { close(w.stopCh) })
	<-w.doneCh
}

func (w *piSessionWatcher) note(msg string) {
	if w.onNote != nil {
		w.onNote(msg)
	}
}

func (w *piSessionWatcher) run(ctx context.Context) {
	defer close(w.doneCh)

	bound := false
	disabled := false
	boundID := ""
	boundPath := ""
	var lastSize int64
	var lastMod time.Time

	bind := func(name, id string, advanced bool) {
		bound = true
		boundID = id
		boundPath = filepath.Join(w.dir, name)
		if info, err := os.Stat(boundPath); err == nil {
			lastSize = info.Size()
			lastMod = info.ModTime()
		}
		w.note("pi liveness: watching session " + id)
		// Advancement between process launch and the bind poll is real session
		// activity (a fresh session's creation, or a resumed session's new
		// events): credit it so the silence clock reflects the true last
		// activity instead of waiting one extra poll interval.
		if advanced {
			w.onActivity(ActivitySession)
		}
	}
	disable := func(reason string) {
		disabled = true
		w.note(reason + "; stdout-only liveness")
	}

	ticker := time.NewTicker(piSessionWatchPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return
		case <-ticker.C:
		}
		if disabled {
			return
		}

		if !bound {
			if w.resumeID != "" {
				matches := w.matchResumeFiles()
				if len(matches) > 1 {
					disable("pi liveness: multiple session files match the resumed session id")
					continue
				}
				if len(matches) == 1 {
					launchState, existed := w.baseline[matches[0].name]
					advanced := !existed || launchState.size != matches[0].state.size || !launchState.mod.Equal(matches[0].state.mod)
					bind(matches[0].name, w.resumeID, advanced)
				}
				continue
			}
			current := w.snapshotSessionFiles()
			var fresh []string
			for name := range current {
				if _, ok := w.baseline[name]; !ok {
					fresh = append(fresh, name)
				}
			}
			if len(fresh) > 1 {
				disable("pi liveness: multiple new session files for this worktree, binding ambiguous")
				continue
			}
			if len(fresh) == 1 {
				candidateID := piSessionFileID(fresh[0])
				if headerID := w.sessionID(); headerID != "" && headerID != candidateID {
					disable("pi liveness: session header does not match the new session file")
					continue
				}
				// A fresh session file did not exist at launch: its creation is
				// this invocation's own session advancement.
				bind(fresh[0], candidateID, true)
			}
			continue
		}

		// Bound: a late-arriving stdout header that names a different session
		// proves the file we bound is not this invocation's; fail closed.
		if headerID := w.sessionID(); headerID != "" && headerID != boundID {
			disable("pi liveness: session header does not match the bound session file")
			continue
		}
		info, err := os.Stat(boundPath)
		if err != nil {
			continue
		}
		if info.Size() != lastSize || !info.ModTime().Equal(lastMod) {
			lastSize = info.Size()
			lastMod = info.ModTime()
			w.onActivity(ActivitySession)
		}
	}
}

// snapshotSessionFiles returns the name→advancement-state map of pi-shaped
// session files in the watch dir. A missing or unreadable dir is empty,
// never an error: absence of evidence keeps the conservative path.
func (w *piSessionWatcher) snapshotSessionFiles() map[string]piSessionFileState {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return map[string]piSessionFileState{}
	}
	out := make(map[string]piSessionFileState, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || piSessionFileID(entry.Name()) == "" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		out[entry.Name()] = piSessionFileState{size: info.Size(), mod: info.ModTime()}
	}
	return out
}

type piResumeMatch struct {
	name  string
	state piSessionFileState
}

func (w *piSessionWatcher) matchResumeFiles() []piResumeMatch {
	var matches []piResumeMatch
	for name, state := range w.snapshotSessionFiles() {
		if piSessionFileID(name) == w.resumeID {
			matches = append(matches, piResumeMatch{name: name, state: state})
		}
	}
	return matches
}
