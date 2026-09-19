package procreap

import (
	"strings"
	"time"
)

// processState is one process-table entry as the descendant probe needs it.
type processState struct {
	PID     int
	PPID    int
	PGID    int
	Stat    string
	Elapsed time.Duration
}

// HasLiveDescendantSince reports whether pid currently has a running
// descendant that started at or after since: a process whose parent chain
// reaches pid, or a member of the process group pid leads. Zombies do not
// count because they are finished work nobody has reaped yet.
//
// It answers "is this agent still waiting on something it launched" for the
// invocation stall budget. A long tool call (a test suite, a build) produces
// no bytes on the agent's own stdout or stderr until it returns, so output
// alone cannot tell that wait from a wedged agent; a live child can. The
// start bound is what separates that tool work from helpers an agent keeps
// alive for its whole turn (an ACP agent under acpx, stdio MCP servers):
// those start before the agent's output they would otherwise outlive.
// Process ages have whole-second resolution, so a process started within
// the second before since still counts.
// A failure to read the process table reports false, so an unreadable host
// never extends a budget.
func HasLiveDescendantSince(pid int, since time.Time) bool {
	if pid <= 1 {
		return false
	}
	procs, err := listProcessStates()
	if err != nil {
		return false
	}
	return hasLiveDescendant(pid, procs, time.Since(since))
}

func hasLiveDescendant(root int, procs []processState, maxAge time.Duration) bool {
	byPID := make(map[int]processState, len(procs))
	for _, p := range procs {
		byPID[p.PID] = p
	}
	for _, p := range procs {
		if p.PID == root || p.PID <= 1 || strings.HasPrefix(p.Stat, "Z") || p.Elapsed > maxAge {
			continue
		}
		if p.PGID == root {
			return true
		}
		seen := map[int]bool{p.PID: true}
		for parent := p.PPID; parent > 1 && !seen[parent]; {
			if parent == root {
				return true
			}
			seen[parent] = true
			next, ok := byPID[parent]
			if !ok {
				break
			}
			parent = next.PPID
		}
	}
	return false
}
