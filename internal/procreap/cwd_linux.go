//go:build linux

package procreap

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// processCWDs resolves each pid's working directory from /proc, which is both
// cheaper and more reliable than shelling out on Linux.
func processCWDs(pids []int) (map[int]string, error) {
	cwds := make(map[int]string, len(pids))
	var lookupErr error
	for _, pid := range pids {
		target, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
		if err != nil {
			if !processAliveFunc(pid) {
				continue
			}
			if os.IsNotExist(err) {
				stat, statErr := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
				if statErr == nil {
					end := strings.LastIndex(string(stat), ") ")
					if end >= 0 && strings.HasPrefix(string(stat)[end+2:], "Z ") {
						continue
					}
				}
			}
			lookupErr = errors.Join(lookupErr, fmt.Errorf("read working directory for live process %d: %w", pid, err))
			continue
		}
		cwds[pid] = trimDeletedSuffix(target)
	}
	return cwds, lookupErr
}
