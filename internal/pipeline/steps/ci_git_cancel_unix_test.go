//go:build unix

package steps

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// TestStepGitRunKillsGrandchildOnCancel proves cancellable git subprocesses do
// not leave hooks, credential helpers, or transport workers running after the
// CI step is cancelled.
func TestStepGitRunKillsGrandchildOnCancel(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	binDir := t.TempDir()
	heartbeat := filepath.Join(t.TempDir(), "tick")
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	fakeGit := filepath.Join(binDir, "git")
	script := "#!/bin/sh\n" +
		"i=0; while [ $i -lt 10000 ]; do printf '%s\\n' \"$i\" > \"$HEARTBEAT\"; sleep 0.1; i=$((i+1)); done &\n" +
		"echo $! > \"$PID_FILE\"\n" +
		"wait\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Ctx = ctx
	sctx.Env = []string{"PATH=" + binDir, "HEARTBEAT=" + heartbeat, "PID_FILE=" + pidFile}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = stepGitRun(sctx, "status", "--porcelain")
	}()

	grandchild := waitForIntFile(t, pidFile, 5*time.Second)
	waitForHeartbeatChange(t, heartbeat, 3*time.Second)
	cancel()

	if !heartbeatHoldsWithin(t, heartbeat, 5*time.Second) {
		t.Fatalf("git grandchild pid %d still running after cancellation", grandchild)
	}
	if err := syscall.Kill(grandchild, 0); err != syscall.ESRCH {
		t.Fatalf("git grandchild pid %d not reaped after cancel (kill -0: %v); want ESRCH", grandchild, err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stepGitRun did not return after cancellation")
	}
}
