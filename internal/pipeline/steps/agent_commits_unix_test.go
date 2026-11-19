//go:build unix

package steps

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCommitAgentFixes_CancelStopsWaitingForSigner(t *testing.T) {
	t.Parallel()
	sctx, dir, _ := agentCommitContext(t, nil)
	agentCommit(t, dir, "one.txt", "first")
	agentHead := gitCmd(t, dir, "rev-parse", "HEAD")
	signer := filepath.Join(t.TempDir(), "signer")
	script := "#!/bin/sh\nprintf '1\\n' > \"$0.ready\"\nwhile [ ! -e \"$0.release\" ]; do sleep 0.05; done\nexit 1\n"
	if err := os.WriteFile(signer, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "config", "gpg.format", "ssh")
	gitCmd(t, dir, "config", "user.signingkey", signer)
	gitCmd(t, dir, "config", "gpg.ssh.program", signer)
	gitCmd(t, dir, "config", "commit.gpgsign", "true")

	ctx, cancel := context.WithCancel(sctx.Ctx)
	sctx.Ctx = ctx
	done := make(chan struct{})
	var rewriteErr error
	go func() {
		defer close(done)
		_, rewriteErr = commitAgentFixesWithResult(sctx, types.StepReview, "repair", "fallback", nil)
	}()
	t.Cleanup(func() {
		cancel()
		if err := os.WriteFile(signer+".release", nil, 0o600); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("rewrite did not return after releasing the signer")
		}
	})

	waitForIntFile(t, signer+".ready", 5*time.Second)
	cancel()
	select {
	case <-done:
		if rewriteErr == nil {
			t.Fatal("cancelled rewrite succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled rewrite is still waiting for the signer")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != agentHead {
		t.Fatalf("HEAD = %s, want the agent's commit %s untouched", got, agentHead)
	}
}
