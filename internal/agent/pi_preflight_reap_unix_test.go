//go:build unix

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A timed-out preflight must terminate Pi's descendants, not only its direct
// process. The fake Pi records a background child's PID and waits; after the
// probe deadline, that PID must already be gone when the call returns.
func TestPiAgent_ProbeTimeoutReapsChildProcess(t *testing.T) {
	setPiProbeTimeout(t, 100*time.Millisecond)
	workDir := t.TempDir()
	pidFile := filepath.Join(workDir, "probe-child.pid")
	bin := writeFakePi(t, t.TempDir(), `#!/bin/sh
( sleep 120 >/dev/null 2>&1 ) &
echo $! > "`+pidFile+`"
wait
`, "")
	pa := &piAgent{bin: bin, extraArgs: []string{"--model", "sonnet"}}

	outcome := pa.probeModelResolution(context.Background(), workDir, true)
	if !strings.Contains(outcome.detail, "did not finish") {
		t.Fatalf("probe outcome = %+v, want timeout diagnosis", outcome)
	}
	childPID := waitForPidFile(t, pidFile, 5*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	if !pidGoneWithin(childPID, 5*time.Second) {
		t.Fatalf("probe child pid %d survived the timeout", childPID)
	}

	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("probe did not record child pid: %v", err)
	}
}
