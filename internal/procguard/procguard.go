// Package procguard is a least-privilege boundary that stops a spawned gate
// agent (review/fix/test/document/lint/CI) from broadly signalling host
// processes that live outside the validation run's process scope.
//
// # Why this exists
//
// Native agents run with the maintainer's credentials and are launched with
// permission prompts disabled, so when an agent decides to "clean up watchers"
// it can run a broad command such as `pkill -f fm-watch.sh` or
// `killall fm-watch.sh`. Those tools signal every matching PID owned by the
// same uid, regardless of who spawned it - which is how a test-fix agent once
// killed a Firstmate watcher process running completely outside the no-mistakes
// worktree and cancelled an unrelated migration. Prompt wording asking the
// agent to "only target the validation copy" proved insufficient.
//
// # What this enforces
//
// Every native agent is launched inside its own process group (Setpgid, see
// internal/shellenv.ConfigureShellCommand). That group *is* the validation
// scope: the agent, its shells, its test runners, and any child it spawns share
// it, while unrelated host processes (a firstmate watcher, another lane's
// daemon) do not. procguard interposes drop-in `kill`/`pkill`/`killall` shims
// ahead of the real tools on the agent's PATH (see Install and AugmentPATH).
// When the agent invokes one, the shim resolves the concrete set of target
// PIDs, compares each target's process group against the guard's own group
// (the scope), and:
//
//   - delivers the signal when every target is in scope (so a run can still
//     reap its own descendants - `pkill -f <its-own-test-worker>` keeps
//     working), and
//   - refuses the whole command, signalling nothing, when any target is out of
//     scope or when the invocation uses a selector the guard cannot bound
//     safely (fail closed).
//
// Decide is the pure, platform-independent decision core and is unit-tested
// exhaustively; the OS-specific snapshot, signal delivery, install, and dispatch
// live in the build-tagged files.
//
// # Threat model and platform boundary
//
// This contains a *cooperative-but-fallible* agent that picks too broad a
// cleanup command - the observed incident. It is defence-in-depth, not a
// sandbox: because the agent runs as the same uid with no privilege separation,
// a determined caller can still bypass PATH interposition via the shell builtin
// `kill` (bash/zsh), an absolute path (`/bin/kill`), or a direct kill(2)
// syscall. Full containment against that requires OS-native isolation - macOS
// Seatbelt signal denial (see SeatbeltSignalProfile), a Linux PID namespace, or
// running the agent under a dedicated uid - which is documented as the stronger,
// platform-specific hardening layer. Windows has no pkill/killall analog on the
// agent PATH and is not shimmed.
package procguard

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Process is a snapshot of one host process, as read from `ps`. Comm is the
// argv[0] token (used for name matching by pkill/killall); Args is the full
// command line (used for `pkill -f` pattern matching).
type Process struct {
	PID  int
	PGID int
	Comm string
	Args string
}

// Outcome is the guard's verdict for one invocation.
type Outcome int

const (
	// OutcomeAllow: every target is in scope; deliver Signal to Targets.
	OutcomeAllow Outcome = iota
	// OutcomeRefuseOutOfScope: at least one target is outside the run's process
	// scope. Signal nothing; this is the observed incident being blocked.
	OutcomeRefuseOutOfScope
	// OutcomeRefuseUnsupported: the invocation uses a shape/selector the guard
	// cannot bound to a definite target set. Fail closed - signal nothing.
	OutcomeRefuseUnsupported
	// OutcomeNoMatch: a pkill/killall pattern matched no processes. Mirror the
	// real tools, which exit non-zero without signalling anything.
	OutcomeNoMatch
	// OutcomePassthrough: a read-only invocation (list signals, --help,
	// --version). Delegate to the real tool unchanged.
	OutcomePassthrough
)

// Decision is the result of Decide. On OutcomeAllow, Targets holds the values to
// pass to kill(2): a positive PID, 0 (the caller's own group), or a negative
// -PGID (an in-scope process group).
type Decision struct {
	Tool      string
	Outcome   Outcome
	Signal    string // "" => default TERM; else a validated numeric or SIG-less name
	Targets   []int
	Offending []Process
	Reason    string
}

// knownSignalNames is the set of signal names (without the SIG prefix) the guard
// recognizes. It is used only to tell a signal token (`-TERM`, `-9`) apart from
// a flag token and to validate an explicit signal; the exact number is resolved
// per-platform at delivery time. An unrecognized signal name never widens the
// target set, so misclassifying one only affects which signal an in-scope kill
// delivers, never whether an out-of-scope process is spared.
var knownSignalNames = map[string]struct{}{
	"HUP": {}, "INT": {}, "QUIT": {}, "ILL": {}, "TRAP": {}, "ABRT": {}, "IOT": {},
	"BUS": {}, "FPE": {}, "KILL": {}, "USR1": {}, "SEGV": {}, "USR2": {}, "PIPE": {},
	"ALRM": {}, "TERM": {}, "STKFLT": {}, "CHLD": {}, "CLD": {}, "CONT": {}, "STOP": {},
	"TSTP": {}, "TTIN": {}, "TTOU": {}, "URG": {}, "XCPU": {}, "XFSZ": {}, "VTALRM": {},
	"PROF": {}, "WINCH": {}, "IO": {}, "POLL": {}, "PWR": {}, "SYS": {}, "EMT": {},
	"INFO": {}, "LOST": {}, "UNUSED": {},
}

// IsGuardTool reports whether name (a path or bare command) is one of the tools
// procguard shims. The set is deliberately small: the broad, name-based
// signalling tools that caused the incident.
func IsGuardTool(name string) bool {
	switch filepath.Base(name) {
	case "kill", "pkill", "killall":
		return true
	}
	return false
}

// Decide is the pure decision core. tool is the invoked command basename, args
// are the arguments after it, scopePGID is the guard's own process group (the
// validation scope), and procs is a snapshot of host processes.
func Decide(tool string, args []string, scopePGID int, procs []Process) Decision {
	tool = filepath.Base(tool)
	// A scope of 0 (unknown) or 1 (init / no dedicated group) is not a safe
	// boundary: signalling within it could reach arbitrary host processes.
	// Fail closed rather than guess.
	if scopePGID <= 1 {
		return refuse(tool, fmt.Sprintf(
			"cannot determine a safe process scope (pgid=%d); refusing to signal", scopePGID))
	}
	switch tool {
	case "kill":
		return decideKill(args, scopePGID, procs)
	case "pkill":
		return decidePkill(args, scopePGID, procs)
	case "killall":
		return decideKillall(args, scopePGID, procs)
	default:
		return refuse(tool, "unsupported tool")
	}
}

func refuse(tool, reason string) Decision {
	return Decision{Tool: tool, Outcome: OutcomeRefuseUnsupported, Reason: reason}
}

func passthrough(tool string) Decision {
	return Decision{Tool: tool, Outcome: OutcomePassthrough}
}

// decideKill handles `kill [-s sig | -sig] pid...`. Per POSIX the signal, if
// present, is only the first option; every remaining token is a target. A
// target is a positive PID, 0 (caller's group), -1 (every process), or -N (the
// process group N).
func decideKill(args []string, scopePGID int, procs []Process) Decision {
	signal := ""
	i := 0
	if i < len(args) {
		switch a := args[i]; {
		case a == "-l" || a == "-L" || a == "--help" || a == "-h" || a == "--version" || a == "-V":
			return passthrough("kill")
		case a == "-s" || a == "--signal":
			if i+1 >= len(args) {
				return refuse("kill", "missing signal for "+a)
			}
			spec, ok := parseSignalToken(args[i+1])
			if !ok {
				return refuse("kill", "unrecognized signal "+args[i+1])
			}
			signal = spec
			i += 2
		case strings.HasPrefix(a, "--signal="):
			spec, ok := parseSignalToken(strings.TrimPrefix(a, "--signal="))
			if !ok {
				return refuse("kill", "unrecognized signal in "+a)
			}
			signal = spec
			i++
		case a == "--":
			i++
		case strings.HasPrefix(a, "-") && a != "-":
			// First option that is not -s/--signal: it is the signal iff its body
			// parses as one (`-9`, `-TERM`). A leading `-1` here is the signal 1
			// (HUP), never the "all processes" target, matching kill(1).
			spec, ok := parseSignalToken(a[1:])
			if !ok {
				return refuse("kill", "unrecognized signal/flag "+a)
			}
			signal = spec
			i++
		}
	}

	targets := args[i:]
	if len(targets) == 0 {
		return refuse("kill", "no target pid given")
	}

	var deliver []int
	var offending []Process
	var reasons []string
	for _, tok := range targets {
		if tok == "--" {
			continue
		}
		n, err := strconv.Atoi(tok)
		if err != nil {
			return refuse("kill", "unsupported target operand "+tok)
		}
		switch {
		case n == -1:
			reasons = append(reasons, "-1 (every process) is outside the validation scope")
		case n == 0:
			// The caller's own process group == the scope.
			deliver = append(deliver, 0)
		case n < 0:
			if pgid := -n; pgid == scopePGID {
				deliver = append(deliver, n)
			} else {
				reasons = append(reasons, fmt.Sprintf("process group %d is outside the validation scope (pgid %d)", pgid, scopePGID))
			}
		default:
			p, found := findProc(procs, n)
			if !found {
				reasons = append(reasons, fmt.Sprintf("cannot verify the process group of pid %d", n))
				continue
			}
			if p.PGID == scopePGID {
				deliver = append(deliver, n)
			} else {
				offending = append(offending, p)
			}
		}
	}

	if len(offending) > 0 || len(reasons) > 0 {
		return Decision{
			Tool:      "kill",
			Outcome:   OutcomeRefuseOutOfScope,
			Signal:    signal,
			Offending: offending,
			Reason:    refusalReason("kill", offending, reasons, scopePGID),
		}
	}
	return Decision{Tool: "kill", Outcome: OutcomeAllow, Signal: signal, Targets: deliver}
}

// decidePkill handles `pkill [-signal] [-f] [-x] pattern...`. Only the signal,
// -f (match the full command line), and -x (exact match) are modelled; every
// other flag (`-u`, `-P`, `-s`, ...) can widen or narrow the target set in ways
// the guard cannot reproduce, so it fails closed.
func decidePkill(args []string, scopePGID int, procs []Process) Decision {
	signal := ""
	matchFull := false
	exact := false
	var patterns []string
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--":
			patterns = append(patterns, args[i+1:]...)
			i = len(args)
			continue
		case a == "-h" || a == "--help" || a == "-V" || a == "--version":
			return passthrough("pkill")
		case a == "--signal":
			if i+1 >= len(args) {
				return refuse("pkill", "missing signal for --signal")
			}
			spec, ok := parseSignalToken(args[i+1])
			if !ok {
				return refuse("pkill", "unrecognized signal "+args[i+1])
			}
			signal = spec
			i += 2
			continue
		case strings.HasPrefix(a, "--signal="):
			spec, ok := parseSignalToken(strings.TrimPrefix(a, "--signal="))
			if !ok {
				return refuse("pkill", "unrecognized signal in "+a)
			}
			signal = spec
			i++
			continue
		case strings.HasPrefix(a, "-") && a != "-":
			body := a[1:]
			if spec, ok := parseSignalToken(body); ok {
				signal = spec
				i++
				continue
			}
			if setPkillLetters(body, &matchFull, &exact) {
				i++
				continue
			}
			return refuse("pkill", "unsupported flag "+a+
				"; narrow to an explicit in-scope target (the guard only models -f/-x and a signal)")
		default:
			patterns = append(patterns, a)
			i++
		}
	}

	if len(patterns) == 0 {
		return refuse("pkill", "no pattern operand")
	}
	matched, err := matchPkill(procs, patterns, matchFull, exact)
	if err != nil {
		return refuse("pkill", err.Error())
	}
	return scopeMatched("pkill", signal, matched, scopePGID)
}

// decideKillall handles `killall [-signal] name...`, matching each name exactly
// against a process's executable basename, like killall(1). Any flag other than
// a signal fails closed.
func decideKillall(args []string, scopePGID int, procs []Process) Decision {
	signal := ""
	var names []string
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--":
			names = append(names, args[i+1:]...)
			i = len(args)
			continue
		case a == "-l" || a == "--help" || a == "--version":
			return passthrough("killall")
		case a == "-s" || a == "--signal":
			if i+1 >= len(args) {
				return refuse("killall", "missing signal for "+a)
			}
			spec, ok := parseSignalToken(args[i+1])
			if !ok {
				return refuse("killall", "unrecognized signal "+args[i+1])
			}
			signal = spec
			i += 2
			continue
		case strings.HasPrefix(a, "--signal="):
			spec, ok := parseSignalToken(strings.TrimPrefix(a, "--signal="))
			if !ok {
				return refuse("killall", "unrecognized signal in "+a)
			}
			signal = spec
			i++
			continue
		case strings.HasPrefix(a, "-") && a != "-":
			if spec, ok := parseSignalToken(a[1:]); ok {
				signal = spec
				i++
				continue
			}
			return refuse("killall", "unsupported flag "+a+
				"; narrow to an explicit in-scope target (the guard only models a signal and names)")
		default:
			names = append(names, a)
			i++
		}
	}

	if len(names) == 0 {
		return refuse("killall", "no process name operand")
	}
	matched := matchKillall(procs, names)
	return scopeMatched("killall", signal, matched, scopePGID)
}

// scopeMatched partitions matched processes by scope and builds the shared
// pkill/killall verdict: refuse the whole command if any match is out of scope.
func scopeMatched(tool, signal string, matched []Process, scopePGID int) Decision {
	if len(matched) == 0 {
		return Decision{Tool: tool, Outcome: OutcomeNoMatch, Signal: signal, Reason: "no matching processes"}
	}
	var deliver []int
	var offending []Process
	for _, p := range matched {
		if p.PGID == scopePGID {
			deliver = append(deliver, p.PID)
		} else {
			offending = append(offending, p)
		}
	}
	if len(offending) > 0 {
		return Decision{
			Tool:      tool,
			Outcome:   OutcomeRefuseOutOfScope,
			Signal:    signal,
			Offending: offending,
			Reason:    refusalReason(tool, offending, nil, scopePGID),
		}
	}
	return Decision{Tool: tool, Outcome: OutcomeAllow, Signal: signal, Targets: deliver}
}

// setPkillLetters applies a run of clustered single-letter pkill flags (e.g.
// "xf"). It returns false if body contains any letter that is not a modelled
// flag, so the caller can fail closed.
func setPkillLetters(body string, matchFull, exact *bool) bool {
	if body == "" {
		return false
	}
	for _, r := range body {
		switch r {
		case 'f':
			*matchFull = true
		case 'x':
			*exact = true
		default:
			return false
		}
	}
	return true
}

func matchPkill(procs []Process, patterns []string, matchFull, exact bool) ([]Process, error) {
	res := make([]regexp.Regexp, 0, len(patterns))
	for _, pat := range patterns {
		expr := pat
		if exact {
			expr = "^(?:" + pat + ")$"
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %v", pat, err)
		}
		res = append(res, *re)
	}
	var matched []Process
	for _, p := range procs {
		subject := p.Comm
		if matchFull {
			subject = p.Args
		}
		for i := range res {
			if res[i].MatchString(subject) {
				matched = append(matched, p)
				break
			}
		}
	}
	return matched, nil
}

func matchKillall(procs []Process, names []string) []Process {
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[n] = struct{}{}
	}
	var matched []Process
	for _, p := range procs {
		base := filepath.Base(p.Comm)
		if _, ok := want[base]; ok {
			matched = append(matched, p)
		}
	}
	return matched
}

func findProc(procs []Process, pid int) (Process, bool) {
	for _, p := range procs {
		if p.PID == pid {
			return p, true
		}
	}
	return Process{}, false
}

// parseSignalToken reports whether tok (a signal spec without a leading dash) is
// a valid signal: all digits, or a signal name with an optional SIG prefix. The
// returned spec is normalized (digits kept as-is; names uppercased, SIG stripped)
// for the platform delivery layer.
func parseSignalToken(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	if isAllDigits(tok) {
		return tok, true
	}
	name := strings.TrimPrefix(strings.ToUpper(tok), "SIG")
	if _, ok := knownSignalNames[name]; ok {
		return name, true
	}
	return "", false
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func refusalReason(tool string, offending []Process, extra []string, scopePGID int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "no-mistakes: refusing %s: it would signal process(es) outside this validation run's scope (pgid %d).", tool, scopePGID)
	for _, p := range offending {
		fmt.Fprintf(&b, "\n  out of scope: pid %d (pgid %d) %s", p.PID, p.PGID, p.Args)
	}
	for _, r := range extra {
		fmt.Fprintf(&b, "\n  %s", r)
	}
	b.WriteString("\nTarget only processes this run spawned (they share its process group), or use an explicit in-scope pid.")
	return b.String()
}
