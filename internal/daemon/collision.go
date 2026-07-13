package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// daemonProcessInfo describes a running `no-mistakes daemon run --root <root>`
// process discovered via the OS process list.
type daemonProcessInfo struct {
	PID  int
	Root string // raw --root value exactly as it appeared on the command line
}

// daemonListDaemonProcesses enumerates no-mistakes managed-daemon processes. It
// is a package var so tests can stub the process list deterministically.
var daemonListDaemonProcesses = listDaemonProcesses

// errDaemonCollisionHealthy signals that Start refused to launch because a
// healthy daemon for the same logical root is already running under a different
// path spelling (e.g. a symlinked NM_HOME).
var errDaemonCollisionHealthy = errors.New("daemon already running")

// daemonProcessLineSplitter extracts a (pid, command line) pair from one line
// of platform-specific process-listing output.
type daemonProcessLineSplitter func(line string) (pid int, command string, ok bool)

// parseDaemonProcessOutput walks a process listing and returns every line that
// looks like `no-mistakes daemon run --root <root>`, extracting the pid and the
// raw --root value. The splitter normalizes each platform's output into a pid
// plus the full command line.
func parseDaemonProcessOutput(output string, split daemonProcessLineSplitter) []daemonProcessInfo {
	var infos []daemonProcessInfo
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		pid, command, ok := split(line)
		if !ok {
			continue
		}
		if !looksLikeDaemonRunCommand(command) {
			continue
		}
		root, hasRoot := extractRootFromCommand(command)
		if !hasRoot {
			continue
		}
		infos = append(infos, daemonProcessInfo{PID: pid, Root: root})
	}
	return infos
}

// looksLikeDaemonRunCommand reports whether a command line is a no-mistakes
// daemon invocation (`... daemon run ...`) with an explicit root.
func looksLikeDaemonRunCommand(command string) bool {
	tokens := splitCommandLineTokens(command)
	hasDaemon, hasRun := false, false
	for _, token := range tokens {
		switch strings.ToLower(token) {
		case "daemon":
			hasDaemon = true
		case "run":
			hasRun = true
		}
	}
	return hasDaemon && hasRun
}

// extractRootFromCommand pulls the --root value out of a command line, accepting
// both `--root <value>` and `--root=<value>`, with or without shell quoting.
func extractRootFromCommand(command string) (string, bool) {
	tokens := splitCommandLineTokens(command)
	for i := 0; i < len(tokens); i++ {
		switch {
		case tokens[i] == "--root":
			if i+1 < len(tokens) {
				return tokens[i+1], true
			}
			return "", false
		case strings.HasPrefix(tokens[i], "--root="):
			return strings.TrimPrefix(tokens[i], "--root="), true
		}
	}
	return "", false
}

// splitCommandLineTokens splits a command line into argv-style tokens, honoring
// double-quote grouping and backslash escapes the way a POSIX shell / Windows
// command line roughly would. It is intentionally permissive: the goal is to
// recover the --root argument from ps/CIM output, not to be a flawless shell
// parser.
func splitCommandLineTokens(command string) []string {
	var tokens []string
	var current strings.Builder
	inQuotes := false
	escaped := false
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	for i := 0; i < len(command); i++ {
		ch := command[i]
		switch {
		case escaped:
			current.WriteByte(ch)
			escaped = false
		case ch == '\\':
			escaped = true
		case ch == '"':
			inQuotes = !inQuotes
		case (ch == ' ' || ch == '\t') && !inQuotes:
			flush()
		default:
			current.WriteByte(ch)
		}
	}
	flush()
	return tokens
}

// reconcileCollidingDaemons detects no-mistakes daemons already running for the
// same logical root as p but started under a different path spelling (e.g. a
// symlinked or relative NM_HOME). The socket-keyed health check in Start cannot
// see those: paths.Paths stores the raw root string and derives its socket from
// it, so two spellings of the same directory produce two different sockets.
//
// Behaviour:
//   - No collision: return nil so Start proceeds.
//   - Healthy collision (a daemon already serves this root): refuse to start a
//     duplicate.
//   - Stale collision (process exists but is unresponsive, e.g. a crash-looping
//     managed unit whose binary died): kill the processes, stop the managed
//     service so systemd's Restart=always cannot immediately revive the dead
//     binary, and clear failed-unit state. Start then installs and starts a
//     fresh daemon.
//
// Enumeration failures fail open (a warning is logged) so an environment
// without ps/CIM does not lose the existing pidfile and socket guards.
func reconcileCollidingDaemons(p *paths.Paths) error {
	want := canonicalRoot(p.Root())
	if want == "" {
		return nil
	}
	procs, err := daemonListDaemonProcesses()
	if err != nil {
		slog.Warn("daemon collision check skipped: process enumeration failed", "error", err)
		return nil
	}

	self := os.Getpid()
	type candidate struct {
		pid  int
		root string
	}
	var matches []candidate
	for _, info := range procs {
		if info.PID == self {
			continue
		}
		if info.Root == "" || canonicalRoot(info.Root) != want {
			continue
		}
		matches = append(matches, candidate{pid: info.PID, root: info.Root})
	}
	if len(matches) == 0 {
		return nil
	}

	for _, m := range matches {
		if alive, _ := daemonHealthCheck(paths.WithRoot(m.root)); alive {
			slog.Info("daemon already running under alternate root path", "pid", m.pid, "root", m.root)
			return fmt.Errorf("%w (pid %d)", errDaemonCollisionHealthy, m.pid)
		}
	}

	for _, m := range matches {
		if killErr := daemonKillPID(m.pid); killErr != nil {
			slog.Warn("kill stale colliding daemon", "pid", m.pid, "error", killErr)
		}
	}
	// Stop the managed unit so Restart=always cannot revive a dead binary while
	// Start re-installs and re-starts it. Best-effort: install/start below will
	// reinstall the unit even if this returns an error.
	if _, err := stopManagedService(p); err != nil {
		slog.Warn("stop managed service before fresh daemon start", "error", err)
	}
	resetFailedManagedService(p)
	for _, m := range matches {
		cleanupDaemonArtifacts(paths.WithRoot(m.root))
	}
	cleanupDaemonArtifacts(p)
	return nil
}
