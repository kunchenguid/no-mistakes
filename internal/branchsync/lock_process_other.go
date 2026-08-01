//go:build plan9

package branchsync

func processAlive(pid int) bool {
	return pid > 0
}
