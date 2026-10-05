//go:build unix

package procreap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

const cwdLookupTimeout = 10 * time.Second

func listProcesses() ([]Process, error) {
	cmd := exec.Command(psExecutable(), "-ww", "-o", "pid=,ppid=,pgid=,etime=,command=", "-u", strconv.Itoa(os.Geteuid()))
	cmd.Env = cEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	procs := parseProcessTable(string(out))
	lines := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			lines++
		}
	}
	if lines == 0 || lines != len(procs) {
		return nil, fmt.Errorf("incomplete process table")
	}
	// Keep command/CWD access account-scoped, but ownership expansion and
	// ancestor protection need identities even after a tool changes its UID.
	states, err := listProcessStates()
	if err != nil {
		return nil, err
	}
	known := make(map[int]bool, len(procs))
	for _, p := range procs {
		known[p.PID] = true
	}
	for _, p := range states {
		if !known[p.PID] {
			procs = append(procs, Process{PID: p.PID, PPID: p.PPID, PGID: p.PGID, metadataOnly: true})
		}
	}
	return procs, nil
}

// listProcessStates reads pid, parent, group, and state for every process.
func listProcessStates() ([]processState, error) {
	cmd := exec.Command(psExecutable(), "-eo", "pid=,ppid=,pgid=,stat=")
	cmd.Env = cEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	var procs []processState
	lines := 0
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		lines++
		if len(fields) != 4 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		pgid, pgidErr := strconv.Atoi(fields[2])
		if pidErr != nil || ppidErr != nil || pgidErr != nil || pid <= 0 {
			continue
		}
		procs = append(procs, processState{PID: pid, PPID: ppid, PGID: pgid, Stat: fields[3]})
	}
	if lines == 0 || lines != len(procs) {
		return nil, fmt.Errorf("incomplete process identity table")
	}
	return procs, nil
}

// parseProcessTable turns `ps -eo pid=,ppid=,pgid=,etime=,command=` output
// into Process values.
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

func lsofCWDs(pids []int) (map[int]string, error) {
	if len(pids) == 0 {
		return nil, nil
	}
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return nil, fmt.Errorf("find working directory reader: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cwdLookupTimeout)
	defer cancel()
	ids := make([]string, 0, len(pids))
	for _, pid := range pids {
		ids = append(ids, strconv.Itoa(pid))
	}
	cmd := exec.CommandContext(ctx, lsof, "-a", "-d", "cwd", "-Fpn", "-p", strings.Join(ids, ","))
	cmd.Env = cEnv()
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("read process working directories: %w", ctx.Err())
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, fmt.Errorf("read process working directories: %w", err)
		}
	}
	cwds := parseLsofCWD(string(out))
	var lookupErr error
	for _, pid := range pids {
		if !filepath.IsAbs(cwds[pid]) && processAliveFunc(pid) {
			lookupErr = errors.Join(lookupErr, fmt.Errorf("working directory unavailable for live process %d", pid))
		}
	}
	return cwds, lookupErr
}
