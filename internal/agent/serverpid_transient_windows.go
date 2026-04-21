//go:build windows

package agent

import (
	"errors"
	"syscall"
)

// isTransientPIDOpenError reports whether err is the brief Windows sharing
// collision that can surface while the writer's MoveFileEx swaps a fresh
// PID file into place.
func isTransientPIDOpenError(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case syscall.Errno(32):
		return true
	}
	return false
}
