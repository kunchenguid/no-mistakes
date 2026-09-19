//go:build windows

package pipeline

import "errors"

var sampleAgentProcess = sampleAgentProcessOS

func sampleAgentProcessOS(pid int) (uint64, string, error) {
	return 0, "", errors.New("process CPU sampling unavailable")
}
