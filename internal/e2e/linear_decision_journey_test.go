//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Drive a selected review fix followed by a documentation edit through the
// real CLI, hook, daemon, worktree, and push. The canned agent supplies only
// responses and edits, not pipeline decisions.
func TestSelectedFixFollowedByDocumentEditDoesNotRestartReview(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "linear.yaml")
	content := `actions:
  - match: "Fix-round provenance:"
    structured:
      findings: []
      summary: "fix verified"
      risk_level: low
      risk_rationale: "no remaining issue"
      risk_scope: source-or-external
  - match: "Investigate previous review findings"
    edits:
      - path: feature.txt
        new: "corrected feature\n"
    structured:
      findings: []
      summary: "fixed"
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: "fix-feature"
          severity: warning
          file: feature.txt
          line: 1
          description: "Correct the feature value"
          action: ask-user
      summary: "needs correction"
      risk_level: medium
      risk_rationale: "incorrect value"
      risk_scope: source-or-external
  - match: "Find what this change made stale"
    edits:
      - path: README.md
        new: "# Corrected feature documentation\n"
    structured:
      findings: []
      summary: "documentation updated"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no remaining risks"
      risk_scope: source-or-external
      tested: ["fixture validation"]
      testing_summary: "fixture validation"
      scenarios:
        - name: "fixture journey"
          result: pass
          live: true
          evidence: "fixture validation"
          reason: ""
      verdict: go
      artifacts: []
      title: "feat: corrected feature"
      body: "## Summary\nCorrected feature"
`
	if err := os.WriteFile(scenario, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	h.CommitChange("linear-init", "seed.txt", "seed\n", "seed")
	initDir := h.AddWorktree("linear-init")
	if out, err := h.RunInDir(initDir, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	branch := "feature/linear-decision"
	h.CommitChange(branch, "feature.txt", "incorrect feature\n", "feature")
	worktree := h.AddWorktree(branch)
	gate, err := h.RunInDir(worktree, "axi", "run", "--intent", "Correct the feature and document it")
	if err != nil || !strings.Contains(gate, "fix-feature") {
		t.Fatalf("initial review gate: %v\n%s", err, gate)
	}
	result, err := h.RunInDir(worktree, "axi", "respond", "--action", "fix", "--findings", "fix-feature")
	if err != nil {
		t.Fatalf("selected fix: %v\n%s", err, result)
	}
	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status %s, error %v, response %s", run.Status, run.Error, result)
	}
	invocations := h.AgentInvocations()
	var reviewCount int
	var testDecision, documentDecision bool
	for _, inv := range invocations {
		if strings.Contains(inv.Prompt, "Review the code changes and return structured findings") {
			reviewCount++
		}
		if strings.Contains(inv.Prompt, "You are validating a code change by driving the product itself") && strings.Contains(inv.Prompt, "Correct the feature value") {
			testDecision = true
		}
		if strings.Contains(inv.Prompt, "Find what this change made stale") && strings.Contains(inv.Prompt, "Correct the feature value") {
			documentDecision = true
		}
	}
	if reviewCount != 2 {
		t.Errorf("review turns = %d, want initial and fix rereview only", reviewCount)
	}
	if !testDecision || !documentDecision {
		t.Errorf("decision reached subsequent steps: test=%v document=%v", testDecision, documentDecision)
	}
	if got := h.UpstreamBranchSHA(branch); got != run.HeadSHA {
		t.Errorf("published head %s, want %s", got, run.HeadSHA)
	}
	for _, stepName := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush} {
		step, ok := findStep(run.Steps, stepName)
		if !ok || step.Status != types.StepStatusCompleted {
			t.Errorf("step %s not completed: %+v", stepName, step)
		}
	}
	doc, err := h.runGit(context.Background(), h.UpstreamDir, "show", "refs/heads/"+branch+":README.md")
	if err != nil || string(doc) != "# Corrected feature documentation\n" {
		t.Errorf("published post-review documentation = %q, error %v", doc, err)
	}
	feature, err := h.runGit(context.Background(), h.UpstreamDir, "show", "refs/heads/"+branch+":feature.txt")
	if err != nil || string(feature) != "corrected feature\n" {
		t.Errorf("published selected fix = %q, error %v", feature, err)
	}
	t.Logf("completed run %s; review turns %d; selected fix %q; post-review documentation %q; decision in Test %v, Document %v; published head %s", run.ID, reviewCount, feature, doc, testDecision, documentDecision, run.HeadSHA)
}
