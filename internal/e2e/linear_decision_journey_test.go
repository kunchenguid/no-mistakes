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

// Drive a selected review fix followed by a documentation edit and a lint
// fix turn through the real CLI, hook, daemon, worktree, and push. The canned
// agent supplies only responses and edits, not pipeline decisions.
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
  - match: "Fix the lint issues in this repository"
    edits:
      - path: lint.ok
        new: "lint clean\n"
    structured:
      summary: "create lint sentinel"
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
	// A trusted lint command that fails until the fix turn creates its
	// sentinel gives the Lint step a gate and a fix turn of its own, so the
	// journey can check that step's rendered prompt too.
	pushMainRepoConfig(t, h, "ignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\nallow_repo_commands: true\ncommands:\n  lint: 'test -f lint.ok'\n")
	branch := "feature/linear-decision"
	h.CommitChange(branch, "feature.txt", "incorrect feature\n", "feature")
	worktree := h.AddWorktree(branch)
	gate, err := h.RunInDir(worktree, "axi", "run", "--intent", "Preserve the original feature value and document it")
	if err != nil || !strings.Contains(gate, "fix-feature") {
		t.Fatalf("initial review gate: %v\n%s", err, gate)
	}
	result, err := h.RunInDir(worktree, "axi", "respond", "--action", "fix", "--findings", "fix-feature")
	if err != nil {
		t.Fatalf("selected fix: %v\n%s", err, result)
	}
	gated := waitForStepStatus(t, h, branch, types.StepLint, types.StepStatusAwaitingApproval, 90*time.Second)
	if gated == nil {
		t.Fatal("lint step never parked on the failing lint command")
	}
	h.Respond(gated.ID, types.StepLint, types.ActionFix)
	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status %s, error %v, response %s", run.Status, run.Error, result)
	}
	invocations := h.AgentInvocations()
	var reviewCount int
	var testDecision, documentDecision, lintDecision bool
	for _, inv := range invocations {
		if strings.Contains(inv.Prompt, "Review the code changes and return structured findings") {
			reviewCount++
		}
		// The selected correction contradicts the original intent. Each later
		// prompt must carry the fix, precedence over intent, and guidance not
		// to undo what the human chose.
		if strings.Contains(inv.Prompt, "You are validating a code change by driving the product itself") {
			testDecision = hasJourneyDecisionContext(inv.Prompt)
		}
		if strings.Contains(inv.Prompt, "Find what this change made stale") {
			documentDecision = hasJourneyDecisionContext(inv.Prompt)
		}
		if strings.Contains(inv.Prompt, "Fix the lint issues in this repository") {
			lintDecision = hasJourneyDecisionContext(inv.Prompt)
		}
	}
	if reviewCount != 2 {
		t.Errorf("review turns = %d, want initial and fix rereview only", reviewCount)
	}
	if !testDecision || !documentDecision || !lintDecision {
		t.Errorf("decision plus respect guidance reached subsequent steps: test=%v document=%v lint=%v", testDecision, documentDecision, lintDecision)
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
	lintSentinel, err := h.runGit(context.Background(), h.UpstreamDir, "show", "refs/heads/"+branch+":lint.ok")
	if err != nil || string(lintSentinel) != "lint clean\n" {
		t.Errorf("published lint fix = %q, error %v", lintSentinel, err)
	}
	t.Logf("completed run %s; review turns %d; selected fix %q; post-review documentation %q; decision in Test %v, Document %v, Lint %v; published head %s", run.ID, reviewCount, feature, doc, testDecision, documentDecision, lintDecision, run.HeadSHA)
}

func hasJourneyDecisionContext(prompt string) bool {
	for _, part := range []string{
		"Preserve the original feature value",
		"review round 1 user chose to fix",
		"Correct the feature value",
		"A recorded decision SUPERSEDES conflicting user-intent wording",
		"Never revert, undo, or work around a recorded human decision",
	} {
		if !strings.Contains(prompt, part) {
			return false
		}
	}
	return true
}
