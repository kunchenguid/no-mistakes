//go:build !windows

package agent

import (
	"os"
	"testing"
)

func TestIsTransientPIDOpenError_NonWindowsAlwaysFalse(t *testing.T) {
	if isTransientPIDOpenError(nil) {
		t.Fatalf("nil error should not be transient")
	}
	if isTransientPIDOpenError(os.ErrPermission) {
		t.Fatalf("permission error should not be transient on this platform")
	}
}

func TestIsTransientPIDReplaceError_NonWindowsAlwaysFalse(t *testing.T) {
	if isTransientPIDReplaceError(nil) {
		t.Fatalf("nil error should not be transient")
	}
	if isTransientPIDReplaceError(os.ErrPermission) {
		t.Fatalf("permission error should not be transient on this platform")
	}
}
