//go:build !unix

package procreap

// On Windows every no-mistakes subprocess is confined to a job object
// (internal/shellenv), which the kernel tears down with the whole tree - a
// descendant cannot escape it the way a setsid(2) call escapes a POSIX process
// group. There is therefore nothing for an identity-based sweep to find, and
// the platform layer reports an empty process table so Sweep is a no-op.

type procSignal int

const (
	sigTerm procSignal = iota
	sigKill
)

func listProcesses() ([]Process, error) { return nil, nil }

func processCWDs(pids []int) map[int]string { return nil }

func processAlive(pid int) bool { return false }

func signalProcess(pid int, sig procSignal) error { return nil }

func signalGroup(pgid int, sig procSignal) error { return nil }
