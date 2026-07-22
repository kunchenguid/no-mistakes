//go:build e2e

package e2e

import (
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
)

// TestMain recovers any stale temporary-daemon inventory left by a prior
// interrupted suite, runs the package tests, then reaps again on exit.
//
// This is the in-process recovery boundary. It does not survive SIGKILL of
// the test process; scripts/e2e.sh EXIT/INT/TERM trap covers that case for
// the wrapper shell, and the next suite's pre-reap covers SIGKILL of the
// wrapper itself via the on-disk inventory.
func TestMain(m *testing.M) {
	if inv, err := e2edaemon.Open(); err == nil {
		_ = inv.ReapAll()
	}
	code := m.Run()
	if inv, err := e2edaemon.Open(); err == nil {
		_ = inv.ReapAll()
	}
	os.Exit(code)
}
