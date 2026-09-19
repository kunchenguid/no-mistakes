package procreap

import "strings"

// processState is one process-table entry as the descendant probe needs it.
type processState struct {
	PID  int
	PPID int
	PGID int
	Stat string
}

// LiveDescendants returns the running descendants of pid: processes whose
// parent chain reaches pid, and members of the process group pid leads.
// Zombies are left out because they are finished work nobody has reaped yet.
//
// The invocation stall budget compares two of these sets to tell whether a
// quiet agent is still waiting on a tool call it launched (a test suite, a
// build) or is wedged next to helpers it keeps alive for its whole turn (an
// ACP agent under acpx, stdio MCP servers). A PID set rather than process
// start times is compared because ps reports ages in whole seconds, which
// cannot order a helper started just before the agent's output against a
// tool started just after it.
func LiveDescendants(pid int) (map[int]bool, error) {
	if pid <= 1 {
		return map[int]bool{}, nil
	}
	procs, err := listProcessStates()
	if err != nil {
		return nil, err
	}
	return liveDescendants(pid, procs), nil
}

func liveDescendants(root int, procs []processState) map[int]bool {
	byPID := make(map[int]processState, len(procs))
	for _, p := range procs {
		byPID[p.PID] = p
	}
	found := make(map[int]bool)
	for _, p := range procs {
		if p.PID == root || p.PID <= 1 || strings.HasPrefix(p.Stat, "Z") {
			continue
		}
		if p.PGID == root {
			found[p.PID] = true
			continue
		}
		seen := map[int]bool{p.PID: true}
		for parent := p.PPID; parent > 1 && !seen[parent]; {
			if parent == root {
				found[p.PID] = true
				break
			}
			seen[parent] = true
			next, ok := byPID[parent]
			if !ok {
				break
			}
			parent = next.PPID
		}
	}
	return found
}
