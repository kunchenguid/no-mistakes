//go:build unix

package procreap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type procSignal = syscall.Signal

const (
	sigTerm = syscall.SIGTERM
	sigKill = syscall.SIGKILL
)

// cwdLookupTimeout bounds the external cwd lookup. It is a diagnostic read on
// a cleanup path, so a wedged or missing helper must degrade to "found
// nothing" rather than stall daemon startup or a run's teardown.
const cwdLookupTimeout = 10 * time.Second

// listProcesses reads the whole process table. `ps` is used rather than /proc
// so one implementation covers macOS and Linux, matching how the daemon's
// other process inspection works (internal/daemon/proc_unix.go).
func listProcesses() ([]Process, error) {
	cmd := exec.Command(psExecutable(), "-ww", "-eo", "pid=,ppid=,pgid=,etime=,command=")
	cmd.Env = cEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	return parseProcessTable(string(out)), nil
}

// parseProcessTable turns `ps -eo pid=,ppid=,pgid=,etime=,command=` output
// into Process values. Lines it cannot parse are skipped: a partially
// readable table is still useful, and an unreadable line must never abort the
// sweep.
func parseProcessTable(out string) []Process {
	var procs []Process
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		pgid, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		elapsed, err := parseETime(fields[3])
		if err != nil {
			continue
		}
		// Rejoin the command from the original line so embedded runs of
		// whitespace inside arguments survive.
		idx := strings.Index(line, fields[3])
		command := strings.TrimSpace(line[idx+len(fields[3]):])
		procs = append(procs, Process{
			PID:     pid,
			PPID:    ppid,
			PGID:    pgid,
			Command: command,
			Elapsed: elapsed,
		})
	}
	return procs
}

// parseETime parses the POSIX `ps etime` format: [[DD-]HH:]MM:SS.
func parseETime(value string) (time.Duration, error) {
	days := 0
	rest := value
	if dash := strings.Index(rest, "-"); dash >= 0 {
		d, err := strconv.Atoi(rest[:dash])
		if err != nil {
			return 0, fmt.Errorf("parse etime %q: %w", value, err)
		}
		days = d
		rest = rest[dash+1:]
	}
	parts := strings.Split(rest, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("parse etime %q: unexpected field count", value)
	}
	nums := make([]int, len(parts))
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return 0, fmt.Errorf("parse etime %q: %w", value, err)
		}
		nums[i] = n
	}
	hours := 0
	if len(nums) == 3 {
		hours = nums[0]
		nums = nums[1:]
	}
	total := time.Duration(days)*24*time.Hour +
		time.Duration(hours)*time.Hour +
		time.Duration(nums[0])*time.Minute +
		time.Duration(nums[1])*time.Second
	return total, nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	// EPERM means the process exists but belongs to somebody else, which is
	// not a process we could have leaked; treating it as alive keeps us from
	// escalating to SIGKILL against it.
	return errors.Is(err, syscall.EPERM)
}

func signalProcess(pid int, sig procSignal) error {
	if pid <= 1 {
		return nil
	}
	return syscall.Kill(pid, sig)
}

func signalGroup(pgid int, sig procSignal) error {
	if pgid <= 1 {
		return nil
	}
	return syscall.Kill(-pgid, sig)
}

func psExecutable() string {
	if path, err := exec.LookPath("ps"); err == nil {
		return path
	}
	if _, err := os.Stat("/bin/ps"); err == nil {
		return "/bin/ps"
	}
	return "ps"
}

func cEnv() []string {
	env := append([]string(nil), os.Environ()...)
	return append(env, "LC_ALL=C", "LANG=C")
}

// trimDeletedSuffix drops the marker the kernel appends to the cwd of a
// process standing in a directory that has since been removed. That is
// precisely the leaked-process case, so the path must stay usable.
func trimDeletedSuffix(path string) string {
	return strings.TrimSuffix(strings.TrimSpace(path), " (deleted)")
}

func parseLsofCWD(out string) map[int]string {
	cwds := make(map[int]string)
	pid := 0
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			parsed, err := strconv.Atoi(strings.TrimSpace(line[1:]))
			if err != nil {
				pid = 0
				continue
			}
			pid = parsed
		case 'n':
			if pid <= 0 {
				continue
			}
			cwds[pid] = trimDeletedSuffix(line[1:])
			pid = 0
		}
	}
	return cwds
}

func lsofCWDs(pids []int) map[int]string {
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), cwdLookupTimeout)
	defer cancel()
	ids := make([]string, 0, len(pids))
	for _, pid := range pids {
		ids = append(ids, strconv.Itoa(pid))
	}
	cmd := exec.CommandContext(ctx, lsof, "-a", "-d", "cwd", "-Fpn", "-p", strings.Join(ids, ","))
	cmd.Env = cEnv()
	// lsof exits nonzero when some of the pids are gone, which is expected
	// while sweeping a dying tree; whatever it printed is still valid.
	out, _ := cmd.Output()
	return parseLsofCWD(string(out))
}
