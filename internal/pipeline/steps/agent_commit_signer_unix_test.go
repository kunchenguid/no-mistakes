//go:build unix

package steps

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCommitAgentFixes_EncryptedSSHKeyFailsWithoutPrompt(t *testing.T) {
	t.Parallel()
	sshKeygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not available")
	}
	session, err := exec.Command("ps", "-o", "sess=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil || strings.TrimSpace(string(session)) == "" {
		t.Skipf("ps cannot report the parent session: %v", err)
	}
	sctx, dir, _ := agentCommitContext(t, nil)
	agentCommit(t, dir, "one.txt", "first")
	agentHead := gitCmd(t, dir, "rev-parse", "HEAD")
	fixture := t.TempDir()
	key := filepath.Join(fixture, "key")
	if out, err := exec.Command(sshKeygen, "-q", "-t", "ed25519", "-N", "fixture passphrase", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	signer := filepath.Join(fixture, "signer")
	script := `#!/bin/sh
if [ "$1" != "-Y" ]; then
	printf '%s\n' 'fixture passphrase'
	exit 0
fi
session=$(ps -o sess= -p "$$" | tr -d '[:space:]')
if [ -z "$session" ] || [ "$session" = "$TEST_PARENT_SESSION" ]; then
	echo "signer inherited the parent session" >&2
	exit 1
fi
exec "$TEST_SSH_KEYGEN" "$@"
`
	if err := os.WriteFile(signer, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "config", "gpg.format", "ssh")
	gitCmd(t, dir, "config", "user.signingkey", key)
	gitCmd(t, dir, "config", "gpg.ssh.program", signer)
	gitCmd(t, dir, "config", "commit.gpgsign", "true")
	sctx.Env = append(sctx.Env, "LC_ALL=C", "SSH_AUTH_SOCK=", "SSH_ASKPASS="+signer, "SSH_ASKPASS_REQUIRE=force",
		"TEST_SSH_KEYGEN="+sshKeygen, "TEST_PARENT_SESSION="+strings.TrimSpace(string(session)))
	ctx, cancel := context.WithTimeout(sctx.Ctx, 10*time.Second)
	defer cancel()
	sctx.Ctx = ctx

	_, err = commitAgentFixesWithResult(sctx, types.StepReview, "repair", "fallback", nil)
	if err == nil || !strings.Contains(err.Error(), "incorrect passphrase") {
		t.Fatalf("rewrite error = %v, want an incorrect passphrase error", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != agentHead {
		t.Fatalf("HEAD = %s, want the agent's commit %s untouched", got, agentHead)
	}
}

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
