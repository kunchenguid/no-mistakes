//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
)

// reraiseIntentEndMarker is the published end-of-Intent line the PR step
// writes after the Intent text (docs: reference/pipeline-steps.md, "PR").
const reraiseIntentEndMarker = "[//]: # (no-mistakes-intent-end)"

// publishedIntentSection returns the body's Intent section from the
// "## Intent" line through the end marker line, or "" when either is absent.
func publishedIntentSection(body string) string {
	start := strings.Index(body, "## Intent\n")
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], "\n"+reraiseIntentEndMarker)
	if end < 0 {
		return ""
	}
	return body[start : start+end+1+len(reraiseIntentEndMarker)]
}

// TestPRReRaiseKeepsTheOriginalIntentJourney drives the change through the
// real binary, daemon, gate hook, and the stateful fake gh that remembers PR
// bodies:
//
//   - run 1 (`axi run --intent`) opens the PR with an Intent that itself
//     contains a heading spelled like a generated section (`## Testing`);
//   - run 2 pushes a new commit with a different `--intent`; the PR's Intent
//     must survive byte for byte while the pipeline-owned attestation moves
//     to the new head;
//   - adversarial: a markerless (pre-upgrade) live body has its Intent
//     replaced by the re-raising run's intent, and the PR step log says why.
func TestPRReRaiseKeepsTheOriginalIntentJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	statePath := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/reraise-intent"
	h.CommitChange(branch, "reraise.txt", "one\n", "add reraise fixture")
	wt := h.AddWorktree(branch)

	// Run 1: open the PR.
	const originalIntent = "ORIGINAL-INTENT open this PR to add the reraise fixture\n\n## Testing\n\n- run make e2e before merging"
	if out, err := h.RunInDir(wt, "axi", "run", "--intent", originalIntent, "--skip", "ci"); err != nil {
		t.Fatalf("axi run (create): %v\n%s", err, out)
	}
	first := waitPublished(t, h, branch, "", "create")
	created := livePR(t, statePath, branch)
	saveSuppliedEvidence(t, "reraise-1-created.md", created.Body)
	originalSection := publishedIntentSection(created.Body)
	if originalSection == "" || !strings.Contains(originalSection, "ORIGINAL-INTENT") || !strings.Contains(originalSection, "## Testing\n\n- run make e2e before merging") {
		t.Fatalf("first publish has no complete marked Intent section:\n%s", created.Body)
	}
	t.Logf("run 1 %s published Intent:\n%s", first.ID, originalSection)

	// Run 2: a new commit and a different intent re-raise the open PR.
	ctx := context.Background()
	if out, err := h.runGit(ctx, wt, "commit", "--allow-empty", "-m", "follow-up change"); err != nil {
		t.Fatalf("follow-up commit: %v\n%s", err, out)
	}
	headOut, err := h.runGit(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	newHead := strings.TrimSpace(string(headOut))
	const followUpIntent = "FOLLOW-UP-INTENT address review feedback only"
	if out, err := h.RunInDir(wt, "axi", "run", "--intent", followUpIntent, "--skip", "ci"); err != nil {
		t.Fatalf("axi run (re-raise): %v\n%s", err, out)
	}
	second := waitPublished(t, h, branch, first.ID, "re-raise")
	reraised := livePR(t, statePath, branch)
	saveSuppliedEvidence(t, "reraise-2-updated.md", reraised.Body)
	if got := publishedIntentSection(reraised.Body); got != originalSection {
		t.Errorf("re-raise changed the Intent section:\n--- before\n%s\n--- after\n%s", originalSection, got)
	}
	if strings.Contains(reraised.Body, "FOLLOW-UP-INTENT") {
		t.Errorf("re-raise published the follow-up run's intent:\n%s", reraised.Body)
	}
	if strings.Count(reraised.Body, "## Intent\n") != 1 {
		t.Errorf("re-raise body has %d Intent headings, want 1:\n%s", strings.Count(reraised.Body, "## Intent\n"), reraised.Body)
	}
	if !strings.Contains(reraised.Body, `"head_sha":"`+newHead+`"`) {
		t.Errorf("re-raise did not refresh the pipeline attestation to head %s:\n%s", newHead, reraised.Body)
	}
	t.Logf("run 2 %s kept the Intent and attested head %s", second.ID, newHead)

	// Adversarial: strip the marker from the live body (a PR published
	// before this change). The pipeline cannot know where the Intent ends,
	// so it must replace it with this run's intent and say so.
	editLivePR(t, statePath, branch, func(p *statefulGHPR) {
		p.Body = strings.Replace(p.Body, "\n\n"+reraiseIntentEndMarker, "", 1)
	})
	if strings.Contains(livePR(t, statePath, branch).Body, reraiseIntentEndMarker) {
		t.Fatalf("marker still present after the simulated legacy edit")
	}
	const legacyIntent = "LEGACY-REPLACEMENT-INTENT third run"
	if out, err := h.RunInDir(wt, "rerun", "--intent", legacyIntent); err != nil {
		t.Fatalf("rerun (legacy): %v\n%s", err, out)
	}
	third := waitPublished(t, h, branch, second.ID, "legacy")
	legacy := livePR(t, statePath, branch)
	saveSuppliedEvidence(t, "reraise-3-markerless-replaced.md", legacy.Body)
	section := publishedIntentSection(legacy.Body)
	if !strings.Contains(section, legacyIntent) || strings.Contains(legacy.Body, "ORIGINAL-INTENT") {
		t.Errorf("markerless body: Intent was not replaced by the run's intent:\n%s", legacy.Body)
	}
	prLog := readStepLog(t, h, third.ID, "pr")
	saveSuppliedEvidence(t, "reraise-3-pr-step.log", prLog)
	if !strings.Contains(prLog, "intent replaced: no intent marker in the published body") {
		t.Errorf("PR step log does not state why the Intent was replaced:\n%s", prLog)
	}
}
