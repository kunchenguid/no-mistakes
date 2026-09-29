package steps

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestRebaseCompletionChecksEmptyDiffAgainstSelectedPRTarget(t *testing.T) {
	dir, mainSHA, headSHA := setupGitRepo(t)
	ensureLocalBranch(t, dir, "develop", headSHA)
	sctx := newTestContext(t, &mockAgent{name: "test"}, dir, mainSHA, headSHA, config.Commands{})
	gitCmd(t, dir, "fetch", "origin", "+refs/heads/develop:refs/remotes/origin/develop")

	outcome, err := updateHeadSHA(context.Background(), sctx, "develop")
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.SkipRemaining {
		t.Fatal("a branch already identical to the selected PR target must skip remaining steps")
	}
}
