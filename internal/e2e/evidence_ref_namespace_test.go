//go:build e2e

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestEvidencePublishStaysOutOfTheBranchNamespace drives two real runs with
// test.evidence.store_in_repo against a GitHub-shaped upstream that already
// carries an unrelated, unmarked code branch named like the evidence default
// (refs/heads/no-mistakes/evidence). Host deploy integrations watch
// refs/heads, so evidence must land under refs/no-mistakes/evidence/ instead,
// the second run must extend the same history, and the pre-existing branch
// must be neither read, refused, nor updated (issue #1466).
func TestEvidencePublishStaysOutOfTheBranchNamespace(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: evidenceBranchConversationScenario(t)})

	const originURL = "https://github.com/owner/no-mistakes.git"
	configureGitURLRewrite(t, h, originURL, h.UpstreamDir)
	if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", originURL); err != nil {
		t.Fatalf("set github-shaped origin: %v\n%s", err, out)
	}
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", filepath.Join(filepath.Dir(h.AgentLog), "gh-evidence.log"))
	t.Setenv("FAKEAGENT_GH_PARENT", "owner/no-mistakes")

	pushMainRepoConfig(t, h, trustedRepoConfigWithConversationAndEvidenceBranch)

	// An unrelated code branch occupying the old evidence branch name.
	if out, err := h.runGit(t.Context(), h.UpstreamDir, "update-ref", "refs/heads/no-mistakes/evidence", "refs/heads/main"); err != nil {
		t.Fatalf("seed unrelated heads branch: %v\n%s", err, out)
	}
	legacyBefore := gitOut(t, h, "rev-parse", "refs/heads/no-mistakes/evidence")

	h.CommitChange("init-evidence-ns", "seed.txt", "seed\n", "seed for the evidence namespace journey")
	if out, err := h.RunInDir(h.AddWorktree("init-evidence-ns"), "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	const evidenceRef = "refs/no-mistakes/evidence/no-mistakes/evidence"
	runOnce := func(branch string) {
		t.Helper()
		h.CommitChange(branch, strings.ReplaceAll(branch, "/", "-")+".txt", "flag = true\n", "add "+branch)
		fw := h.AddWorktree(branch)
		if out, err := h.RunInDir(fw, "axi", "run", "--intent", "wire the feature flag into the config loader"); err != nil {
			t.Fatalf("axi run %s: %v\n%s", branch, err, out)
		}
		if waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second) == nil {
			t.Fatalf("%s: review step never parked for an answer", branch)
		}
		if out, err := h.RunInDir(fw, "axi", "answer",
			"--question", "q1", "--answer", conversationAnswerText, "--by", conversationAnsweredBy); err != nil {
			t.Fatalf("axi answer %s: %v\n%s", branch, err, out)
		}
		if run := h.WaitForRun(branch, 180*time.Second); run.Status != types.RunCompleted {
			t.Fatalf("%s: run status = %s, want completed (error=%v)", branch, run.Status, deref(run.Error))
		}
	}

	runOnce("feature/evidence-ns-one")
	firstTip := gitOut(t, h, "rev-parse", evidenceRef)

	runOnce("feature/evidence-ns-two")
	secondTip := gitOut(t, h, "rev-parse", evidenceRef)

	t.Logf("upstream refs after both runs:\n%s", gitOut(t, h, "for-each-ref", "--format=%(objectname:short) %(refname)"))
	t.Logf("evidence history:\n%s", gitOut(t, h, "log", "--format=%h %p %s", evidenceRef))

	if got := gitOut(t, h, "rev-parse", "refs/heads/no-mistakes/evidence"); got != legacyBefore {
		t.Errorf("unrelated refs/heads/no-mistakes/evidence moved: %s -> %s", legacyBefore, got)
	}
	if heads := gitOut(t, h, "for-each-ref", "--format=%(refname)", "refs/heads/no-mistakes/"); heads != "refs/heads/no-mistakes/evidence" {
		t.Errorf("evidence publication created branches under refs/heads/no-mistakes/:\n%s", heads)
	}
	if secondTip == firstTip {
		t.Fatalf("second run did not publish a new evidence commit")
	}
	if parent := gitOut(t, h, "rev-parse", secondTip+"^"); parent != firstTip {
		t.Errorf("second evidence commit parent = %s, want the first run's tip %s", parent, firstTip)
	}
	if roots := gitOut(t, h, "rev-list", "--max-parents=0", evidenceRef); roots != gitOut(t, h, "rev-parse", firstTip) {
		t.Errorf("evidence history is not a fresh orphan rooted at the first publish: roots %q", roots)
	}
	tree := gitOut(t, h, "ls-tree", "-r", "--name-only", evidenceRef)
	t.Logf("evidence tree:\n%s", tree)
	for _, want := range []string{
		".no-mistakes/evidence/feature/evidence-ns-one/run-transcript.txt",
		".no-mistakes/evidence/feature/evidence-ns-two/run-transcript.txt",
	} {
		if !strings.Contains(tree, want) {
			t.Errorf("evidence tree missing %s", want)
		}
	}
	if strings.Contains(tree, ".no-mistakes.yaml") || strings.Contains(tree, "seed.txt") {
		t.Errorf("evidence history inherited the unrelated heads branch's tree:\n%s", tree)
	}
}

func gitOut(t *testing.T, h *Harness, args ...string) string {
	t.Helper()
	out, err := h.runGit(t.Context(), h.UpstreamDir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
