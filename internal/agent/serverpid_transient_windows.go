//go:build windows

package agent

import (
	"errors"
	"syscall"
)

// isTransientPIDOpenError reports whether err is the brief Windows sharing
// collision that can surface while the writer's MoveFileEx swaps a fresh
// PID file into place. ERROR_SHARING_VIOLATION (32) and ERROR_ACCESS_DENIED
// (5) both clear on the next attempt, so callers should treat them the
// same as a not-yet-created file rather than a hard failure.
func isTransientPIDOpenError(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case syscall.Errno(32), syscall.Errno(5):
		return true
	}
	return false
}
