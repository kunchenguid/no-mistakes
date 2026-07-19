package procguard

import (
	"testing"
)

// scopePGID is the validation run's process group for these tests. Every
// snapshot below places the agent and its descendants in this group; the
// firstmate watcher sits in its own, unrelated group.
const scopePGID = 5000

// snapshot models the incident's host state: a no-mistakes gate agent and its
// own scoped worker, plus an out-of-scope Firstmate watcher (fm-watch.sh) that
// lives entirely outside the validation run.
func snapshot() []Process {
	return []Process{
		{PID: 5000, PGID: 5000, Comm: "claude", Args: "claude -p review"},
		{PID: 5001, PGID: 5000, Comm: "node", Args: "node ./test-worker.js --run"},
		{PID: 5002, PGID: 5000, Comm: "watchexec", Args: "watchexec -- go test ./local-watch"},
		{PID: 9000, PGID: 9000, Comm: "/Users/control/firstmate/bin/fm-watch.sh",
			Args: "/bin/sh /Users/control/firstmate/bin/fm-watch.sh --daemon"},
		{PID: 9100, PGID: 9000, Comm: "sleep", Args: "sleep 60"},
	}
}

func pids(procs []Process) []int {
	out := make([]int, len(procs))
	for i, p := range procs {
		out[i] = p.PID
	}
	return out
}

func containsInt(xs []int, want int) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestDecide_ReproducesObservedBroadKill is the regression for the incident:
// `pkill -f fm-watch.sh` and `killall fm-watch.sh` must be refused, signalling
// nothing, because the watcher is outside the validation run's process scope.
func TestDecide_ReproducesObservedBroadKill(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args []string
	}{
		{"pkill -f fm-watch.sh", "pkill", []string{"-f", "fm-watch.sh"}},
		{"pkill -f fm-watch", "pkill", []string{"-f", "fm-watch"}},
		{"killall fm-watch.sh", "killall", []string{"fm-watch.sh"}},
		{"killall -9 fm-watch.sh", "killall", []string{"-9", "fm-watch.sh"}},
		{"kill out-of-scope pid", "kill", []string{"-9", "9000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.tool, tc.args, scopePGID, snapshot())
			if d.Outcome != OutcomeRefuseOutOfScope {
				t.Fatalf("outcome = %v, want RefuseOutOfScope (must not signal fm-watch.sh)", d.Outcome)
			}
			if len(d.Targets) != 0 {
				t.Fatalf("refused command still produced targets %v", d.Targets)
			}
			if !containsInt(pids(d.Offending), 9000) {
				t.Fatalf("offending = %v, want it to name the out-of-scope watcher pid 9000", pids(d.Offending))
			}
		})
	}
}

// TestDecide_BroadPatternRefusesWholeCommand proves a pattern that matches BOTH
// in-scope and out-of-scope processes refuses the entire command rather than
// silently signalling the in-scope subset.
func TestDecide_BroadPatternRefusesWholeCommand(t *testing.T) {
	// "watch" matches the in-scope watchexec (5002) and, via -f, the
	// out-of-scope fm-watch.sh (9000).
	d := Decide("pkill", []string{"-f", "watch"}, scopePGID, snapshot())
	if d.Outcome != OutcomeRefuseOutOfScope {
		t.Fatalf("outcome = %v, want RefuseOutOfScope", d.Outcome)
	}
	if len(d.Targets) != 0 {
		t.Fatalf("must signal nothing when any match is out of scope; got targets %v", d.Targets)
	}
	if !containsInt(pids(d.Offending), 9000) {
		t.Fatalf("offending = %v, want out-of-scope 9000 flagged", pids(d.Offending))
	}
}

// TestDecide_ScopedCleanupStillWorks proves the guard still allows a run to reap
// its own descendants (they share its process group).
func TestDecide_ScopedCleanupStillWorks(t *testing.T) {
	cases := []struct {
		name        string
		tool        string
		args        []string
		wantTargets []int
		wantSignal  string
	}{
		{"pkill -f own worker", "pkill", []string{"-f", "test-worker"}, []int{5001}, ""},
		{"pkill -f KILL own worker", "pkill", []string{"--signal", "KILL", "-f", "test-worker"}, []int{5001}, "KILL"},
		{"kill scoped pid", "kill", []string{"-TERM", "5001"}, []int{5001}, "TERM"},
		{"kill default signal", "kill", []string{"5002"}, []int{5002}, ""},
		{"kill self group", "kill", []string{"0"}, []int{0}, ""},
		{"kill in-scope group", "kill", []string{"-TERM", "-5000"}, []int{-5000}, "TERM"},
		{"killall scoped name", "killall", []string{"node"}, []int{5001}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.tool, tc.args, scopePGID, snapshot())
			if d.Outcome != OutcomeAllow {
				t.Fatalf("outcome = %v (reason %q), want Allow", d.Outcome, d.Reason)
			}
			if d.Signal != tc.wantSignal {
				t.Fatalf("signal = %q, want %q", d.Signal, tc.wantSignal)
			}
			if len(d.Targets) != len(tc.wantTargets) {
				t.Fatalf("targets = %v, want %v", d.Targets, tc.wantTargets)
			}
			for _, want := range tc.wantTargets {
				if !containsInt(d.Targets, want) {
					t.Fatalf("targets = %v, want to include %d", d.Targets, want)
				}
			}
		})
	}
}

// TestDecide_FailsClosedOnUnmodeledSelectors proves the guard refuses (signals
// nothing) when an invocation uses a selector it cannot faithfully reproduce,
// even when the pattern also names an in-scope process.
func TestDecide_FailsClosedOnUnmodeledSelectors(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args []string
	}{
		{"pkill -u selector", "pkill", []string{"-u", "root", "-f", "test-worker"}},
		{"pkill -P selector", "pkill", []string{"-P", "1", "test-worker"}},
		{"pkill session selector -s", "pkill", []string{"-s", "0", "test-worker"}},
		{"killall -w waiter", "killall", []string{"-w", "node"}},
		{"kill bogus operand", "kill", []string{"-9", "notapid"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.tool, tc.args, scopePGID, snapshot())
			if d.Outcome != OutcomeRefuseUnsupported {
				t.Fatalf("outcome = %v, want RefuseUnsupported (fail closed)", d.Outcome)
			}
			if len(d.Targets) != 0 {
				t.Fatalf("fail-closed refusal produced targets %v", d.Targets)
			}
		})
	}
}

func TestDecide_RefusesEveryProcessTargets(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"kill -TERM -1", []string{"-TERM", "-1"}},
		{"kill out-of-scope group", []string{"-TERM", "-9000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide("kill", tc.args, scopePGID, snapshot())
			if d.Outcome != OutcomeRefuseOutOfScope {
				t.Fatalf("outcome = %v, want RefuseOutOfScope", d.Outcome)
			}
			if len(d.Targets) != 0 {
				t.Fatalf("targets = %v, want none", d.Targets)
			}
		})
	}
}

func TestDecide_NoMatchMirrorsRealTools(t *testing.T) {
	d := Decide("pkill", []string{"-f", "no-such-marker-zzz"}, scopePGID, snapshot())
	if d.Outcome != OutcomeNoMatch {
		t.Fatalf("outcome = %v, want NoMatch", d.Outcome)
	}
}

func TestDecide_PassthroughReadOnly(t *testing.T) {
	cases := []struct {
		tool string
		args []string
	}{
		{"kill", []string{"-l"}},
		{"pkill", []string{"--help"}},
		{"killall", []string{"-l"}},
		{"kill", []string{"--version"}},
	}
	for _, tc := range cases {
		d := Decide(tc.tool, tc.args, scopePGID, snapshot())
		if d.Outcome != OutcomePassthrough {
			t.Fatalf("%s %v: outcome = %v, want Passthrough", tc.tool, tc.args, d.Outcome)
		}
	}
}

// TestDecide_FailsClosedWhenScopeUnknown proves an undeterminable scope (pgid
// 0/1) refuses all signalling rather than guessing.
func TestDecide_FailsClosedWhenScopeUnknown(t *testing.T) {
	for _, pgid := range []int{0, 1} {
		d := Decide("pkill", []string{"-f", "test-worker"}, pgid, snapshot())
		if d.Outcome != OutcomeRefuseUnsupported {
			t.Fatalf("scope %d: outcome = %v, want RefuseUnsupported", pgid, d.Outcome)
		}
	}
}

// TestDecide_UnverifiablePidFailsClosed proves an explicit pid absent from the
// snapshot is refused (we cannot prove it is in scope).
func TestDecide_UnverifiablePidFailsClosed(t *testing.T) {
	d := Decide("kill", []string{"-9", "424242"}, scopePGID, snapshot())
	if d.Outcome != OutcomeRefuseOutOfScope {
		t.Fatalf("outcome = %v, want RefuseOutOfScope for an unverifiable pid", d.Outcome)
	}
}

func TestDecide_SignalParsing(t *testing.T) {
	cases := []struct {
		args   []string
		signal string
	}{
		{[]string{"-9", "5001"}, "9"},
		{[]string{"-KILL", "5001"}, "KILL"},
		{[]string{"-SIGKILL", "5001"}, "KILL"},
		{[]string{"-s", "TERM", "5001"}, "TERM"},
		{[]string{"--signal", "15", "5001"}, "15"},
		{[]string{"--signal=HUP", "5001"}, "HUP"},
	}
	for _, tc := range cases {
		d := Decide("kill", tc.args, scopePGID, snapshot())
		if d.Outcome != OutcomeAllow {
			t.Fatalf("kill %v: outcome = %v, want Allow", tc.args, d.Outcome)
		}
		if d.Signal != tc.signal {
			t.Fatalf("kill %v: signal = %q, want %q", tc.args, d.Signal, tc.signal)
		}
	}
}

func TestIsGuardTool(t *testing.T) {
	for _, name := range []string{"kill", "pkill", "killall", "/usr/bin/pkill", "./killall"} {
		if !IsGuardTool(name) {
			t.Errorf("IsGuardTool(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"no-mistakes", "ps", "git", "node"} {
		if IsGuardTool(name) {
			t.Errorf("IsGuardTool(%q) = true, want false", name)
		}
	}
}
