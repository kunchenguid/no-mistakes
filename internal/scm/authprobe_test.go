package scm

import (
	"os/exec"
	"runtime"
	"testing"
)

// The auth probe distinguishes a definitive "CLI not authenticated" answer (a
// clean non-zero exit) from transient environmental noise (a probe killed by a
// signal or that never started, e.g. an OOM-killed subprocess under memory
// pressure). Only the transient case is retried; a clean non-zero exit is a
// real negative and must be returned immediately without wasting retries.

func requireSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test relies on a POSIX shell")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
}

func TestRunAuthProbe_DoesNotRetryCleanNonZeroExit(t *testing.T) {
	requireSh(t)
	calls := 0
	err := RunAuthProbe(func() *exec.Cmd {
		calls++
		return exec.Command("sh", "-c", "exit 3")
	})
	if err == nil {
		t.Fatal("expected error for clean non-zero exit")
	}
	if calls != 1 {
		t.Fatalf("clean non-zero exit must not be retried: got %d calls, want 1", calls)
	}
}

func TestRunAuthProbe_RetriesTransientKillThenSucceeds(t *testing.T) {
	requireSh(t)
	calls := 0
	err := RunAuthProbe(func() *exec.Cmd {
		calls++
		if calls < 3 {
			// Kill the probe with SIGKILL: reported as "signal: killed", never
			// a clean exit, so it must be treated as transient and retried.
			return exec.Command("sh", "-c", "kill -9 $$")
		}
		return exec.Command("sh", "-c", "exit 0")
	})
	if err != nil {
		t.Fatalf("expected transient kills to be retried to success, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts (2 kills + 1 success), got %d", calls)
	}
}

func TestRunAuthProbe_ExhaustsRetriesOnPersistentKill(t *testing.T) {
	requireSh(t)
	calls := 0
	err := RunAuthProbe(func() *exec.Cmd {
		calls++
		return exec.Command("sh", "-c", "kill -9 $$")
	})
	if err == nil {
		t.Fatal("expected error after retries are exhausted")
	}
	if calls < 2 {
		t.Fatalf("expected a persistent transient failure to be retried, got %d calls", calls)
	}
}
