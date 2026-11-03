//go:build !windows

package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A transport helper (ssh, git-remote-https) outlives a killed git and keeps its
// inherited stderr pipe open, so a read that only kills git never returns. The
// script answers git's `ssh -G` variant probe, records that it was reached, then
// never speaks again.
func TestCIStep_CommitRepair_StalledTransportHelperFailsClosedWithinItsDeadline(t *testing.T) {
	original := publishedHeadResolveWindow
	publishedHeadResolveWindow = time.Second
	t.Cleanup(func() { publishedHeadResolveWindow = original })

	tmp := t.TempDir()
	reached := filepath.Join(tmp, "reached")
	helper := filepath.Join(tmp, "stalled-ssh.sh")
	script := "#!/bin/sh\n[ \"$1\" = -G ] && exit 0\n: > '" + reached + "'\nexec sleep 30\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sctx, dir, _ := agentCommitContext(t, nil)
	sctx.Config.CI.RevalidateRepairs = true
	sctx.Repo.ForkURL = "ssh://stalled.invalid/repo.git"
	sctx.Env = []string{"GIT_SSH_COMMAND=" + helper}
	agentCommit(t, dir, "stalled.txt", "fix(tests): agent subject")
	agentHead := gitCmd(t, dir, "rev-parse", "HEAD")

	done := make(chan error, 1)
	go func() {
		_, err := (&CIStep{}).commitRepair(sctx, "repair", nil)
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("commitRepair is still blocked on the transport helper, the deadline did not end it")
	}

	if _, statErr := os.Stat(reached); statErr != nil {
		t.Fatalf("the stalled transport was never reached, so the test proved nothing: %v", statErr)
	}
	if err == nil || !strings.Contains(err.Error(), "already published") {
		t.Fatalf("err = %v, want a refusal because the published state could not be read", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != agentHead {
		t.Fatalf("HEAD = %s, want the agent's commit %s left untouched", got, agentHead)
	}
}
