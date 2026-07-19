//go:build unix

package procguard

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Exit codes returned by the shim. 0 mirrors a successful signal; 1 mirrors the
// real tools' "no process matched"; 3 and 4 are the guard's own refusals so a
// caller (and its logs) can tell a scope refusal apart from an ordinary miss.
const (
	exitSignalled     = 0
	exitNoMatch       = 1
	exitOutOfScope    = 3
	exitUnsupported   = 4
	exitPassthroughNA = 127
)

// Run executes the guard for one invocation: it establishes the scope from the
// caller's own process group, snapshots host processes, decides, and either
// delivers the signal, refuses, or delegates a read-only op to the real tool.
// It returns the process exit code.
func Run(tool string, args []string) int {
	scopePGID, err := syscall.Getpgid(0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no-mistakes: refusing %s: cannot read own process group: %v\n", tool, err)
		return exitOutOfScope
	}

	procs, snapErr := Snapshot()
	// A failed snapshot must fail closed for anything that consults the process
	// table. Without it we cannot prove a target is in scope, and an empty
	// snapshot would otherwise masquerade as a benign "no match" for pkill/killall
	// while the real tool (reading the live table) could still hit an out-of-scope
	// process. Read-only ops are unaffected because Decide short-circuits them
	// before consulting procs.
	d := Decide(tool, args, scopePGID, procs)
	if snapErr != nil && (d.Outcome == OutcomeAllow || d.Outcome == OutcomeNoMatch) {
		fmt.Fprintf(os.Stderr, "no-mistakes: refusing %s: cannot snapshot processes to verify scope: %v\n", tool, snapErr)
		return exitOutOfScope
	}

	switch d.Outcome {
	case OutcomeAllow:
		return deliver(d)
	case OutcomeNoMatch:
		return exitNoMatch
	case OutcomeRefuseOutOfScope:
		fmt.Fprintln(os.Stderr, d.Reason)
		return exitOutOfScope
	case OutcomeRefuseUnsupported:
		fmt.Fprintf(os.Stderr, "no-mistakes: refusing %s: %s\n", d.Tool, d.Reason)
		return exitUnsupported
	case OutcomePassthrough:
		return passthroughToReal(tool, args)
	default:
		fmt.Fprintf(os.Stderr, "no-mistakes: refusing %s: unclassified decision\n", d.Tool)
		return exitUnsupported
	}
}

// deliver sends the decided signal to each in-scope target. A target that has
// already exited (ESRCH) is not an error - it just vanished after the snapshot.
func deliver(d Decision) int {
	sig := resolveSignal(d.Signal)
	signalled := 0
	var firstErr error
	for _, t := range d.Targets {
		err := syscall.Kill(t, sig)
		switch {
		case err == nil:
			signalled++
		case err == syscall.ESRCH:
			// Raced with exit; nothing to do.
		default:
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		fmt.Fprintf(os.Stderr, "no-mistakes: %s: %v\n", d.Tool, firstErr)
		return exitUnsupported
	}
	if signalled == 0 && (d.Tool == "pkill" || d.Tool == "killall") {
		return exitNoMatch
	}
	return exitSignalled
}

// resolveSignal maps a validated spec (numeric or SIG-less name) to the host's
// signal number, defaulting to SIGTERM.
func resolveSignal(spec string) syscall.Signal {
	if spec == "" {
		return syscall.SIGTERM
	}
	if isAllDigits(spec) {
		n, _ := strconv.Atoi(spec)
		return syscall.Signal(n)
	}
	if n := unix.SignalNum("SIG" + spec); n != 0 {
		return n
	}
	return syscall.SIGTERM
}

// Snapshot lists host processes via ps. Using `-o pid=,pgid=,args=` keeps
// parsing unambiguous: pid and pgid are the first two whitespace fields and the
// remainder (which may contain spaces) is the full command line. Comm is taken
// as the argv[0] token for name matching.
//
// `-ww` disables ps's column truncation. This is a correctness requirement, not
// cosmetics: a truncated args column would hide the tail of a long command line,
// and a `pkill -f` pattern that matches only that hidden tail would look like a
// no-match here while the real pkill (reading full argv) would signal it - a
// scope-verification gap that must not exist.
func Snapshot() ([]Process, error) {
	out, err := exec.Command("ps", "-A", "-ww", "-o", "pid=,pgid=,args=").Output()
	if err != nil {
		return nil, err
	}
	return parsePS(string(out)), nil
}

func parsePS(out string) []Process {
	var procs []Process
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		pgid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		// Rebuild args as everything after the first two fields, preserving
		// internal spacing between argv tokens.
		rest := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
		rest = strings.TrimSpace(strings.TrimPrefix(rest, fields[1]))
		procs = append(procs, Process{
			PID:  pid,
			PGID: pgid,
			Comm: fields[2],
			Args: rest,
		})
	}
	return procs
}

// passthroughToReal execs the real tool for read-only operations. It resolves
// the tool from PATH while skipping the guard's own shim directory so it never
// recurses into itself.
func passthroughToReal(tool string, args []string) int {
	realBin, err := findRealTool(tool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no-mistakes: %s: %v\n", tool, err)
		return exitPassthroughNA
	}
	cmd := exec.Command(realBin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "no-mistakes: %s: %v\n", tool, err)
		return exitPassthroughNA
	}
	return exitSignalled
}

// findRealTool returns the first PATH entry for name that is not inside the
// guard's own shim directory and does not resolve back to this executable.
func findRealTool(name string) (string, error) {
	selfDir := ""
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			selfDir = filepath.Dir(resolved)
		} else {
			selfDir = filepath.Dir(exe)
		}
	}
	shimDir := ""
	if d, err := DefaultBinDir(); err == nil {
		shimDir = d
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if sameDir(dir, shimDir) {
			continue
		}
		cand := filepath.Join(dir, name)
		info, err := os.Stat(cand)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(cand); err == nil && filepath.Dir(resolved) == selfDir {
			continue // a shim pointing back at us
		}
		return cand, nil
	}
	return "", fmt.Errorf("no real %s found on PATH", name)
}

func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	return ea == nil && eb == nil && ra == rb
}
