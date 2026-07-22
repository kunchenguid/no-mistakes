package e2edaemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// processCommandLine returns the command line for pid, or "" if unknown.
func processCommandLine(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid")
	}
	cmd := exec.Command(psPath(), "-p", strconv.Itoa(pid), "-ww", "-o", "command=")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func processAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	// Prefer signal 0 on unix; fall back to ps on platforms without it.
	return processAliveOS(pid)
}

func psPath() string {
	if p, err := exec.LookPath("ps"); err == nil {
		return p
	}
	if _, err := os.Stat("/bin/ps"); err == nil {
		return "/bin/ps"
	}
	return "ps"
}

// MatchesDaemonRoot reports whether pid's argv is a no-mistakes daemon run
// for exactly the given NM_HOME root (bounded ownership check).
func MatchesDaemonRoot(pid int, nmHome string) bool {
	cmd, err := processCommandLine(pid)
	if err != nil || cmd == "" {
		return false
	}
	return commandMatchesDaemonRoot(cmd, nmHome)
}

func commandMatchesDaemonRoot(command, nmHome string) bool {
	if !looksLikeDaemonRun(command) {
		return false
	}
	root, ok := extractRoot(command)
	if !ok {
		return false
	}
	return samePath(root, nmHome)
}

func looksLikeDaemonRun(command string) bool {
	tokens := splitTokens(command)
	hasDaemon, hasRun := false, false
	for _, t := range tokens {
		switch strings.ToLower(t) {
		case "daemon":
			hasDaemon = true
		case "run":
			hasRun = true
		}
	}
	return hasDaemon && hasRun
}

func extractRoot(command string) (string, bool) {
	return extractRootTokens(splitTokens(command))
}

func extractRootTokens(tokens []string) (string, bool) {
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

func splitTokens(command string) []string {
	if runtime.GOOS == "windows" {
		return splitWindowsTokens(command)
	}
	return splitUnixTokens(command)
}

func splitUnixTokens(command string) []string {
	var tokens []string
	var cur strings.Builder
	inQuotes := false
	escaped := false
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(command); i++ {
		ch := command[i]
		switch {
		case escaped:
			cur.WriteByte(ch)
			escaped = false
		case ch == '\\':
			escaped = true
		case ch == '"':
			inQuotes = !inQuotes
		case (ch == ' ' || ch == '\t') && !inQuotes:
			flush()
		default:
			cur.WriteByte(ch)
		}
	}
	flush()
	return tokens
}

func splitWindowsTokens(command string) []string {
	var tokens []string
	for i := 0; i < len(command); {
		for i < len(command) && (command[i] == ' ' || command[i] == '\t') {
			i++
		}
		if i == len(command) {
			break
		}

		var cur strings.Builder
		inQuotes := false
		for i < len(command) {
			if (command[i] == ' ' || command[i] == '\t') && !inQuotes {
				break
			}
			if command[i] == '"' {
				inQuotes = !inQuotes
				i++
				continue
			}
			if command[i] != '\\' {
				cur.WriteByte(command[i])
				i++
				continue
			}

			start := i
			for i < len(command) && command[i] == '\\' {
				i++
			}
			backslashes := i - start
			if i < len(command) && command[i] == '"' {
				cur.WriteString(strings.Repeat(`\`, backslashes/2))
				if backslashes%2 == 0 {
					inQuotes = !inQuotes
				} else {
					cur.WriteByte('"')
				}
				i++
				continue
			}
			cur.WriteString(strings.Repeat(`\`, backslashes))
		}
		tokens = append(tokens, cur.String())
	}
	return tokens
}

func samePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if a == b {
		return true
	}
	// Resolve symlinks when both sides exist (macOS /var vs /private/var).
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA == nil && errB == nil {
		return filepath.Clean(ra) == filepath.Clean(rb)
	}
	return false
}

// FindDaemonsForRoot lists live PIDs whose argv is daemon run --root nmHome.
func FindDaemonsForRoot(nmHome string) ([]int, error) {
	cmd := exec.Command(psPath(), "-ww", "-eo", "pid=,command=")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sep := strings.IndexAny(line, " \t")
		if sep < 0 {
			continue
		}
		pid, err := strconv.Atoi(line[:sep])
		if err != nil || pid <= 0 {
			continue
		}
		command := strings.TrimSpace(line[sep:])
		if commandMatchesDaemonRoot(command, nmHome) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// signalProcess sends sig to pid. Used only after MatchesDaemonRoot.
func signalProcess(pid int, sig os.Signal) error {
	return signalProcessOS(pid, sig)
}

// waitProcessExit waits until pid is gone or timeout.
func waitProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		alive, err := processAlive(pid)
		if err != nil || !alive {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	alive, err := processAlive(pid)
	return err != nil || !alive
}
