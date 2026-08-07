//go:build linux

package procreap

import (
	"os"
	"strconv"
)

// processCWDs resolves each pid's working directory from /proc, which is both
// cheaper and more reliable than shelling out on Linux.
func processCWDs(pids []int) map[int]string {
	cwds := make(map[int]string, len(pids))
	for _, pid := range pids {
		target, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
		if err != nil {
			continue
		}
		cwds[pid] = trimDeletedSuffix(target)
	}
	return cwds
}
