//go:build !windows

package update

import "fmt"

func defaultWindowsExecutablePathForPID(int) (string, error) {
	return "", fmt.Errorf("windows process path lookup unavailable")
}
