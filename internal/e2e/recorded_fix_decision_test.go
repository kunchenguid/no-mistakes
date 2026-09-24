//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Drives the real CLI, daemon, SQLite rounds and Git publication boundary.
// The fixture models an imperfect Test fixer and an independent reviewer;
// it proves orchestration and acceptance, not live-model detection accuracy.
func TestRecordedFixDecisionSurvivesTestAutoFix(t *testing.T) {
	scenario := axiScenario(t)
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	h.CommitChange("init-decisions", "seed.txt", "seed\n", "seed")
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/recorded-decision"
	h.CommitChange(branch, "feature.txt", "original\n", "add original identifier")
	h.CommitChange(branch, ".no-mistakes.yaml", "allow_repo_commands: true\nauto_fix:\n  test: 1\n", "enable Test auto-fix")
	operator := h.AddWorktree(branch)
	out, err := h.RunInDir(operator, "axi", "run", "--intent", "Use the original identifier prefix")
	if err != nil || !strings.Contains(out, "axi-1") {
		t.Fatalf("initial decision gate: %v\n%s", err, out)
	}
	run := h.ActiveRun(branch)
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var roundID string
	for _, step := range steps {
		if step.StepName == types.StepReview {
			rounds, err := database.GetRoundsByStep(step.ID)
			if err != nil || len(rounds) != 1 {
				t.Fatalf("initial review rounds: %v, %v", rounds, err)
			}
			roundID = rounds[0].ID
		}
	}
	decisionID := roundID + "/axi-1"
	content := fmt.Sprintf(`actions:
  - match: "Investigate previous review findings"
    edits:
      - path: feature.txt
        new: "decided\n"
    structured:
      summary: "use the decided prefix"
  - match: "Fix the failing tests in this repository"
    edits:
      - path: feature.txt
        new: "original\n"
    structured:
      summary: "restore original prefix for legacy test"
  - match: "Round 2 (auto_fix)"
    structured:
      findings: []
      summary: "original intent satisfied but decision reversed"
      risk_level: low
      risk_rationale: "original behavior is otherwise clean"
      risk_scope: source-or-external
      decision_reviews:
        - decision_id: %q
          result: contradicted
          evidence: "feature.txt:1 contains original; the Test fix removed the decided prefix required by the recorded fix instruction."
  - match: "auto_selected_to_fix"
    structured: &clean
      findings: []
      summary: "clean"
      risk_level: low
      risk_rationale: "clean"
      risk_scope: source-or-external
      decision_reviews:
        - decision_id: %q
          result: satisfied
          evidence: "feature.txt:1 contains decided after the review fix."
      tested: ["fixture: checked identifier"]
      testing_summary: "fixture validation completed"
      scenarios:
        - name: "fixture identifier check"
          result: pass
          live: true
          evidence: "fixture validation"
          reason: ""
      verdict: go
      artifacts: []
      title: "fix: preserve decided identifier"
      body: "Preserve the recorded decision."
  - match: "You are validating a code change by driving the product itself"
    structured:
      <<: *clean
      findings:
        - id: legacy-prefix
          severity: error
          file: feature.txt
          description: "Legacy identifier test expects original."
          action: auto-fix
      scenarios:
        - name: "fixture legacy test"
          result: fail
          live: true
          evidence: "fixture: expected original, got decided"
          reason: ""
      verdict: no-go
  - structured: *clean
`, decisionID, decisionID)
	if err := os.WriteFile(scenario, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "axi-1", "--instructions", "Use decided as the identifier prefix, superseding the original intent.")
	if err != nil {
		t.Fatalf("respond fix: %v\n%s", err, out)
	}
	if !strings.Contains(out, "gate:") || !strings.Contains(out, "step: review") || !strings.Contains(out, "recorded fix decision") {
		t.Fatalf("Test reversal escaped independent Review:\n%s", out)
	}
	info := h.RunInfo(run.ID)
	worktree := paths.WithRoot(h.NMHome).WorktreeDir(h.repoID(), run.ID)
	actual, readErr := os.ReadFile(filepath.Join(worktree, "feature.txt"))
	if readErr != nil || strings.TrimSpace(string(actual)) != "original" {
		t.Fatalf("fixture did not reverse the decision: %q, %v", actual, readErr)
	}
	t.Logf("Test reverted the recorded decision at %s; independent Review parked before publication", info.HeadSHA)
	if info.Status == types.RunCompleted {
		t.Fatal("run certified a reversal")
	}
	if out, err := h.runGit(t.Context(), h.UpstreamDir, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		t.Fatalf("reversal was published at %s", out)
	}
	var sawTestFix bool
	for _, invocation := range h.AgentInvocations() {
		if strings.Contains(invocation.Prompt, "Fix the failing tests in this repository") {
			sawTestFix = true
		}
	}
	if !sawTestFix {
		t.Fatal("fixture did not exercise Test auto-fix")
	}
}
