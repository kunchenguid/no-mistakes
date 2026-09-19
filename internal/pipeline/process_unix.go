//go:build !windows

package pipeline

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

var sampleAgentProcess = sampleAgentProcessOS

// sampleAgentProcessOS reads the native process state and accumulated CPU time.
// A runnable process with no accumulated CPU time is the measured wedge shape.
func sampleAgentProcessOS(pid int) (uint64, string, error) {
	if pid <= 0 {
		return 0, "", fmt.Errorf("invalid process id %d", pid)
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "time=,state=").Output()
	if err != nil {
		return 0, "", err
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return 0, "", fmt.Errorf("invalid process sample %q", strings.TrimSpace(string(out)))
	}
	return parseProcessCPUTime(fields[0]), fields[1], nil
}

func parseProcessCPUTime(value string) uint64 {
	parts := strings.Split(value, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return ^uint64(0)
	}
	var seconds uint64
	for _, part := range parts {
		v, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return ^uint64(0)
		}
		seconds = seconds*60 + v
	}
	return seconds
}
