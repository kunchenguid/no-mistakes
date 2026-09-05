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

	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

// Unix environment keys are case-sensitive. A differently-cased overlay key
// must coexist with, not replace, the uppercase variable Pi actually reads;
// preflight must resolve the same value as the environment passed to Pi.
func TestPiAgent_EnvironmentLookupMatchesRunOverlayCase(t *testing.T) {
	expectedDir := filepath.Join(t.TempDir(), "expected-agent")
	wrongDir := filepath.Join(t.TempDir(), "wrong-agent")
	t.Setenv("PI_CODING_AGENT_DIR", expectedDir)
	overlay := runenv.Overlay{Set: map[string]string{"pi_coding_agent_dir": wrongDir}}

	applied := overlay.Apply(nil)
	values := make(map[string]string)
	for _, entry := range applied {
		key, value, ok := strings.Cut(entry, "=")
		if ok && (key == "PI_CODING_AGENT_DIR" || key == "pi_coding_agent_dir") {
			values[key] = value
		}
	}
	if got := values["PI_CODING_AGENT_DIR"]; got != expectedDir {
		t.Fatalf("run environment uppercase value = %q, want %q", got, expectedDir)
	}
	if got := values["pi_coding_agent_dir"]; got != wrongDir {
		t.Fatalf("run environment lowercase value = %q, want %q", got, wrongDir)
	}
	if got := piAgentDir(overlay); got != expectedDir {
		t.Fatalf("preflight agent dir = %q, want run environment value %q", got, expectedDir)
	}
}

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
