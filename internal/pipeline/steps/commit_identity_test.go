package steps

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCorrectionCommitIdentityAcrossProducers(t *testing.T) {
	for _, step := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepCI} {
		t.Run(string(step), func(t *testing.T) {
			sctx, dir, _ := agentCommitContext(t, nil)
			identity := git.CommitIdentity{AuthorName: "Submitter", AuthorEmail: "submitter@example.test", CommitterName: "Submitter", CommitterEmail: "submitter@example.test"}
			sctx.Ctx = git.WithCommitIdentity(context.Background(), identity)
			t.Setenv("GIT_AUTHOR_NAME", "Daemon")
			t.Setenv("GIT_AUTHOR_EMAIL", "daemon@example.test")
			t.Setenv("GIT_COMMITTER_NAME", "Daemon")
			t.Setenv("GIT_COMMITTER_EMAIL", "daemon@example.test")
			writeAgentChange(t, dir)
			if step == types.StepCI {
				sctx.Config.CI.RevalidateRepairs = true
				if _, err := (&CIStep{}).commitRepair(sctx, "repair", nil); err != nil {
					t.Fatal(err)
				}
			} else if _, err := commitAgentFixesWithResult(sctx, step, "repair", "fallback", nil); err != nil {
				t.Fatal(err)
			}
			if got := gitCmd(t, dir, "show", "-s", "--format=%an <%ae> | %cn <%ce>"); got != "Submitter <submitter@example.test> | Submitter <submitter@example.test>" {
				t.Fatalf("%s correction identity = %q", step, got)
			}
		})
	}
}

func TestAgentCommitRewritePreservesAuthorAndBindsCommitter(t *testing.T) {
	sctx, dir, _ := agentCommitContext(t, nil)
	identity := git.CommitIdentity{AuthorName: "Submitter", AuthorEmail: "submitter@example.test", CommitterName: "Submitter", CommitterEmail: "submitter@example.test"}
	sctx.Ctx = git.WithCommitIdentity(context.Background(), identity)
	if err := os.WriteFile(filepath.Join(dir, "agent-change"), []byte("repair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The fake agent runs the same subprocess path as step commands. An explicit
	// per-commit author is intentional attribution and survives message shaping.
	if _, err := stepGitRun(sctx, "add", "agent-change"); err != nil {
		t.Fatal(err)
	}
	if _, err := stepGitRun(sctx, "commit", "--author=Contributor <contributor@example.test>", "-m", "agent's message"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_COMMITTER_NAME", "Daemon")
	t.Setenv("GIT_COMMITTER_EMAIL", "daemon@example.test")
	if _, err := commitAgentFixesWithResult(sctx, types.StepReview, "repair", "fallback", nil); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, dir, "show", "-s", "--format=%an <%ae> | %cn <%ce>"); got != "Contributor <contributor@example.test> | Submitter <submitter@example.test>" {
		t.Fatalf("rewritten commit identity = %q", got)
	}
}
