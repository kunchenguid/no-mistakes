//go:build unix && !linux

package procreap

// processCWDs resolves each pid's working directory. Outside Linux there is no
// /proc to read, so one batched lsof call answers for the whole candidate set.
func processCWDs(pids []int) map[int]string {
	if len(pids) == 0 {
		return nil
	}
	return lsofCWDs(pids)
}
