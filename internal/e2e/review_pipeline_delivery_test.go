//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// pipelineOwnedPRIntent is the reported class of acceptance criterion: a PR
// outcome this same pipeline run owns later (push + PR steps), not something
// source review can verify pre-push.
const pipelineOwnedPRIntent = `REQUIRED: Open PR A unmerged`

// deferredPRFinding is the invalid pre-push finding shape reported in the bug:
// the reviewer treats "PR does not exist yet" as an intent contradiction.
const deferredPRFinding = `The required criterion says "Open PR A unmerged," but the PR list returned zero PRs and the target commit is not present on a remote branch. PR A still needs to be opened without merging.`

// writePipelineOwnedPRScenario scripts a review agent that emits the deferred
// pipeline-owned PR finding when the review prompt matches, while every other
// step stays clean so push/PR/CI can still complete.
func writePipelineOwnedPRScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pipeline-owned-pr-scenario.yaml")
	content := `actions:
  - match: "Review the code changes and return structured findings"
    text: "review found missing PR"
    structured:
      findings:
        - id: "intent-missing-pr"
          severity: error
          description: ` + yamlDoubleQuoted(deferredPRFinding) + `
          action: ask-user
      summary: "missing required open PR"
      risk_level: high
      risk_rationale: "required PR criterion not satisfied"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      title: "feat: open PR A"
      body: "## Summary\nOpen PR A unmerged"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write scenario: %v", err)
	}
	return path
}

func writeExternalPRScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "external-pr-scenario.yaml")
	content := `actions:
  - match: "Review the code changes and return structured findings"
    text: "review found external PR issue"
    structured:
      findings:
        - id: "external-pr-closed"
          severity: error
          description: "PR #456 must remain open and unmerged; it is currently closed"
          action: ask-user
      summary: "external PR requirement violated"
      risk_level: high
      risk_rationale: "pre-existing PR requirement not met"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks"
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      title: "feat: change"
      body: "## Summary\nchange"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write scenario: %v", err)
	}
	return path
}

// yamlDoubleQuoted produces a YAML double-quoted scalar for a multi-line-safe
// single-line description string.
func yamlDoubleQuoted(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + replacer.Replace(s) + `"`
}

// TestReviewPipelineOwnedPRCriterionDoesNotPark proves the production path:
// axi run with an authoritative intent that requires a pipeline-owned PR
// outcome, a review agent that emits the reported "PR missing" finding, and
// stage ordering that still runs push/PR after source review. Before the fix
// this parked at review; after, the deferred finding is stripped and the run
// proceeds through PR ownership.
func TestReviewPipelineOwnedPRCriterionDoesNotPark(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writePipelineOwnedPRScenario(t)})

	h.CommitChange("init-delivery", "seed.txt", "seed\n", "seed")
	initWT := h.AddWorktree("init-delivery")
	if out, err := h.RunInDir(initWT, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	h.CommitChange("feature/open-pr-a", "feature.txt", "change for PR A\n", "add feature for PR A")
	fw := h.AddWorktree("feature/open-pr-a")

	out, err := h.RunInDir(fw, "axi", "run", "--intent", pipelineOwnedPRIntent)
	if err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}

	// Must not stop at a review gate solely for the deferred PR claim.
	if strings.Contains(out, "gate:") && strings.Contains(out, "step: review") {
		t.Fatalf("run parked at review gate on pipeline-owned PR criterion; output:\n%s", out)
	}
	if strings.Contains(out, "PR list returned zero") {
		t.Fatalf("deferred PR finding leaked into axi output:\n%s", out)
	}

	completed := h.WaitForRun("feature/open-pr-a", 90*time.Second)
	if completed.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want completed; error=%v\naxi out:\n%s", completed.Status, completed.Error, out)
	}

	// Stage ordering: push and PR (pipeline-owned delivery) still ran after review.
	for _, stepName := range []types.StepName{types.StepReview, types.StepPush, types.StepPR} {
		st, ok := findStep(completed.Steps, stepName)
		if !ok {
			t.Fatalf("missing step %s in run", stepName)
		}
		if st.Status != types.StepStatusCompleted && st.Status != types.StepStatusSkipped {
			t.Errorf("step %s status = %s, want completed or skipped", stepName, st.Status)
		}
	}
	// Review completed without parking; deferred claim must not remain in findings.
	review, ok := findStep(completed.Steps, types.StepReview)
	if !ok {
		t.Fatal("missing review step")
	}
	if review.Status != types.StepStatusCompleted {
		t.Errorf("review status = %s, want completed", review.Status)
	}
	if review.FindingsJSON != nil && strings.Contains(*review.FindingsJSON, "PR list returned zero") {
		t.Errorf("review findings still contain deferred PR claim: %s", *review.FindingsJSON)
	}

	// Prompt boundary is present on the production review path.
	reviewPrompt := findInvocationContaining(h.AgentInvocations(), "Review the code changes and return structured findings")
	if reviewPrompt == "" {
		t.Fatal("no review prompt observed")
	}
	for _, want := range []string{
		"Pipeline phase (review is pre-push)",
		"AUTHORITATIVE acceptance criteria",
		pipelineOwnedPRIntent,
		"Do not treat deferred pipeline-owned delivery outcomes",
	} {
		if !strings.Contains(reviewPrompt, want) {
			t.Errorf("review prompt missing %q; prompt was:\n%s", want, truncate(reviewPrompt, 3000))
		}
	}
}

// TestReviewExternalPRLifecycleStillParks is the negative control: a finding
// about a pre-existing external PR is not suppressed at pre-push review.
func TestReviewExternalPRLifecycleStillParks(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writeExternalPRScenario(t)})

	h.CommitChange("init-ext", "seed.txt", "seed\n", "seed")
	initWT := h.AddWorktree("init-ext")
	if out, err := h.RunInDir(initWT, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	h.CommitChange("feature/external-pr", "feature.txt", "change\n", "change")
	fw := h.AddWorktree("feature/external-pr")

	out, err := h.RunInDir(fw, "axi", "run", "--intent", "REQUIRED: keep PR #456 open and unmerged")
	if err != nil {
		// axi run exits 0 at a gate; if it errors, surface it.
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"gate:",
		"step: review",
		"PR #456",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("axi run gate output missing %q in:\n%s", want, out)
		}
	}
	if gated := waitForStepStatus(t, h, "feature/external-pr", types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second); gated == nil {
		t.Fatal("expected run to park at review for external PR lifecycle finding")
	}
}
