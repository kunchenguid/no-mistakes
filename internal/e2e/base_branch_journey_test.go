//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestBaseBranchOverrideScopesReviewToIntegrationBranch reproduces the original
// reported bug: a GitFlow repo whose GitHub default branch is main but whose
// integration branch is develop. A feature branched one commit ahead of develop
// (but many commits behind main) must be reviewed against develop, not main.
//
// Before the base-branch override, the gate diffed main..HEAD and pulled in the
// entire "develop is ahead of main" backlog. With `init --base-branch develop`
// the review base resolves to the develop tip, so the diff covers only the one
// feature commit and the PR (when a real host is configured) targets develop.
func TestBaseBranchOverrideScopesReviewToIntegrationBranch(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	ctx := context.Background()

	mustGit := func(args ...string) {
		t.Helper()
		if out, err := h.runGit(ctx, h.WorkDir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	commit := func(path, content, message string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(h.WorkDir, path), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		mustGit("add", path)
		mustGit("commit", "-m", message)
	}

	// develop diverges ahead of main with several lead commits - the backlog the
	// buggy main..HEAD diff would wrongly absorb.
	mustGit("checkout", "-b", "develop")
	for i := 1; i <= 3; i++ {
		commit(fmt.Sprintf("lead_%d.txt", i), fmt.Sprintf("develop lead %d\n", i), fmt.Sprintf("develop lead commit %d", i))
	}
	mustGit("push", "origin", "develop")
	developTip := h.WorktreeRefSHA("develop")
	mainTip := h.WorktreeRefSHA("main")
	if developTip == mainTip {
		t.Fatal("precondition failed: develop should be ahead of main")
	}

	out, err := h.Run("init", "--base-branch", "develop")
	if err != nil {
		t.Fatalf("init --base-branch develop: %v\n%s", err, out)
	}

	// Feature is one commit ahead of develop.
	branch := "feature/base-override"
	mustGit("checkout", "-b", branch, "develop")
	commit("feature.txt", "the one real change\n", "the single feature commit")
	featureHead := h.WorktreeRefSHA("HEAD")

	h.PushToGate(branch)
	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	if run.HeadSHA != featureHead {
		t.Fatalf("run head = %s, want feature head %s", run.HeadSHA, featureHead)
	}

	invs := h.AgentInvocations()
	prompt, ok := promptContainingAll(invs, "Review the code changes", "branch: "+branch)
	if !ok {
		t.Fatalf("expected a review prompt for %s, got %d invocations:\n%s", branch, len(invs), summarisePrompts(invs))
	}
	if !strings.Contains(prompt, developTip) {
		t.Errorf("review base should be the develop tip %s (diff = develop..HEAD), prompt:\n%s", developTip, prompt)
	}
	if strings.Contains(prompt, mainTip) {
		t.Errorf("review base must NOT be the main tip %s (that is the 144-commit bug), prompt:\n%s", mainTip, prompt)
	}
	if !strings.Contains(prompt, "default branch: develop") {
		t.Errorf("review prompt should report the integration branch as develop, prompt:\n%s", prompt)
	}
}
