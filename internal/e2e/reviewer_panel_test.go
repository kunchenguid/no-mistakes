//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestReviewerPanelJourney drives the mixed-family review panel end to end with
// zero real API: the harness symlinks claude + codex to the fixture-replaying
// fakeagent, and a global review.reviewers panel makes the review step fan out
// across both families. The review action returns EMPTY-id findings so the
// panel's NormalizeFindings stamps namespaced ids (review-claude-1-1 /
// review-codex-2-1, where the middle ordinal is the reviewer's position in the
// panel) and Source is the family name, which we assert on the persisted,
// post-combine FindingsJSON.
func TestReviewerPanelJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{
		Agent:     "claude",
		Reviewers: []string{"claude", "codex"},
		Scenario:  reviewerPanelScenario(t),
	})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	branch := "reviewer-panel"
	h.CommitChange(branch, "panel.go", "package panel\n\nfunc Panel() {}\n", "add panel change")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("panel run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}

	// Both families must have launched and carried the review prompt for this
	// branch. codex is never the impl agent here, so every codex invocation is
	// a reviewer launch; claude is also the impl agent, so we match the review
	// preamble specifically rather than counting claude launches.
	invs := h.AgentInvocations()
	if !sawReviewInvocation(invs, "codex", branch) {
		t.Fatalf("expected a codex reviewer launch carrying the review prompt for %s; invocations:\n%s", branch, summarisePrompts(invs))
	}
	if !sawReviewInvocation(invs, "claude", branch) {
		t.Fatalf("expected a claude reviewer launch carrying the review prompt for %s; invocations:\n%s", branch, summarisePrompts(invs))
	}

	// The persisted review findings are the attributed union of both reviewers.
	reviewStep, ok := findStep(run.Steps, types.StepReview)
	if !ok {
		t.Fatal("expected review step in panel run")
	}
	if reviewStep.FindingsJSON == nil {
		t.Fatal("expected the panel review step to persist findings JSON")
	}
	findings, err := types.ParseFindingsJSON(*reviewStep.FindingsJSON)
	if err != nil {
		t.Fatalf("parse panel review findings: %v\n%s", err, *reviewStep.FindingsJSON)
	}
	if len(findings.Items) != 2 {
		t.Fatalf("expected 2 attributed panel findings (one per family), got %d: %s", len(findings.Items), *reviewStep.FindingsJSON)
	}

	codexFinding, ok := findingFromSource(findings.Items, "codex")
	if !ok {
		t.Fatalf("expected a finding sourced from codex; got %s", *reviewStep.FindingsJSON)
	}
	if codexFinding.ID != "review-codex-2-1" {
		t.Errorf("codex finding id = %q, want review-codex-2-1 (namespaced from an empty-id finding)", codexFinding.ID)
	}
	claudeFinding, ok := findingFromSource(findings.Items, "claude")
	if !ok {
		t.Fatalf("expected a finding sourced from claude; got %s", *reviewStep.FindingsJSON)
	}
	if claudeFinding.ID != "review-claude-1-1" {
		t.Errorf("claude finding id = %q, want review-claude-1-1 (namespaced from an empty-id finding)", claudeFinding.ID)
	}
}

// TestReviewerPanelSecurityGate proves the review panel is treated as
// code-executing config by EffectiveRepoConfig: a panel on the TRUSTED default
// branch launches every family, while a panel pushed ONLY on a feature branch
// (without allow_repo_commands) is stripped to the single impl agent.
func TestReviewerPanelSecurityGate(t *testing.T) {
	t.Run("trusted_default_branch_panel_launches_both", func(t *testing.T) {
		optOut := false
		h := NewHarness(t, SetupOpts{
			Agent:             "claude",
			RepoReviewers:     []string{"claude", "codex"},
			AllowRepoCommands: &optOut,
			Scenario:          reviewerPanelScenario(t),
		})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		branch := "panel-trusted"
		h.CommitChange(branch, "trusted.go", "package trusted\n\nfunc Trusted() {}\n", "add trusted panel change")
		h.PushToGate(branch)

		run := h.WaitForRun(branch, 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("trusted-panel run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}

		invs := h.AgentInvocations()
		if !sawReviewInvocation(invs, "codex", branch) {
			t.Fatalf("trusted-branch panel should launch the codex family; invocations:\n%s", summarisePrompts(invs))
		}
		if !sawReviewInvocation(invs, "claude", branch) {
			t.Fatalf("trusted-branch panel should launch the claude family; invocations:\n%s", summarisePrompts(invs))
		}
	})

	t.Run("pushed_only_panel_stripped_to_single_agent", func(t *testing.T) {
		// No trusted panel, no global panel, and allow_repo_commands is off. A
		// panel that arrives ONLY on the pushed feature branch must be dropped
		// by EffectiveRepoConfig, leaving the single impl agent to review.
		optOut := false
		h := NewHarness(t, SetupOpts{
			Agent:             "claude",
			AllowRepoCommands: &optOut,
			Scenario:          reviewerPanelScenario(t),
		})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		branch := "panel-pushed-only"
		h.CommitChange(branch, "pushed.go", "package pushed\n\nfunc Pushed() {}\n", "add pushed panel change")
		// The contributor injects a review panel on their own branch alongside
		// the change. Without allow_repo_commands on the trusted branch this
		// must be ignored.
		pushedConfig := "ignore_patterns:\n  - 'vendor/**'\n" + reviewPanelYAML([]string{"claude", "codex"}, 0)
		h.CommitChange(branch, ".no-mistakes.yaml", pushedConfig, "inject review panel on feature branch")
		h.PushToGate(branch)

		run := h.WaitForRun(branch, 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("pushed-only-panel run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}

		// The single impl agent (claude) must still review, but the stripped
		// codex family must never have launched.
		invs := h.AgentInvocations()
		if !sawReviewInvocation(invs, "claude", branch) {
			t.Fatalf("expected the single impl agent to review %s; invocations:\n%s", branch, summarisePrompts(invs))
		}
		for _, inv := range invs {
			if inv.Agent == "codex" {
				t.Fatalf("SECURITY REGRESSION: a feature-branch review panel launched the codex family (%v); review.reviewers must come from the trusted default branch", inv.Args)
			}
		}
	})
}

// TestReviewerPanelFailClosed proves the panel honors the fail-closed default:
// when every reviewer family errors (the shared review prompt routes both to an
// erroring action), the review step and the run fail rather than silently
// dropping reviewers.
func TestReviewerPanelFailClosed(t *testing.T) {
	h := NewHarness(t, SetupOpts{
		Agent:     "claude",
		Reviewers: []string{"claude", "codex"},
		Scenario:  reviewerPanelFailScenario(t),
	})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	branch := "panel-fail-closed"
	h.CommitChange(branch, "failclosed.go", "package failclosed\n\nfunc FailClosed() {}\n", "add fail-closed panel change")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunFailed {
		t.Fatalf("fail-closed panel run status = %s, want failed (error=%v)", run.Status, deref(run.Error))
	}
	reviewStep, ok := findStep(run.Steps, types.StepReview)
	if !ok {
		t.Fatal("expected review step in fail-closed run")
	}
	if reviewStep.Status != types.StepStatusFailed {
		t.Fatalf("expected review step to fail closed, got %s", reviewStep.Status)
	}
}

// sawReviewInvocation reports whether the named agent family launched with the
// review prompt for the given branch.
func sawReviewInvocation(invs []Invocation, agent, branch string) bool {
	for _, inv := range invs {
		if inv.Agent != agent {
			continue
		}
		if strings.Contains(inv.Prompt, "Review the code changes and return structured findings") &&
			strings.Contains(inv.Prompt, "branch: "+branch) {
			return true
		}
	}
	return false
}

func findingFromSource(items []types.Finding, source string) (types.Finding, bool) {
	for _, item := range items {
		if item.Source == source {
			return item, true
		}
	}
	return types.Finding{}, false
}

// reviewerPanelScenario returns a scenario whose review action emits a single
// EMPTY-id, non-blocking finding (so the run completes without gating) while
// every other step gets the standard clean catch-all. The empty id is what lets
// the panel observe NormalizeFindings stamping review-<family>-<ordinal>-N.
func reviewerPanelScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "panel-scenario.yaml")
	// The finding carries an EMPTY id (id: "") plus every other field. codex's
	// real agent rewrites the output schema so all properties are required
	// (codexOutputSchema), so every field must be present; an empty id still
	// passes (string type) yet is what NormalizeFindings stamps into
	// review-<family>-<ordinal>-N, which is exactly the namespacing we assert on.
	content := `actions:
  - match: "Review the code changes and return structured findings with a risk assessment."
    text: "panel reviewer note"
    structured:
      findings:
        - id: ""
          severity: info
          file: "panel.go"
          line: 1
          description: "panel reviewer observation"
          action: no-op
      risk_level: low
      risk_rationale: "informational finding only"
      tested:
        - "fakeagent: simulated review"
      testing_summary: "not run during review"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      title: "feat: fakeagent change"
      body: "## Summary\nfakeagent canned PR body"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write panel scenario: %v", err)
	}
	return path
}

// reviewerPanelFailScenario returns a scenario whose review action makes the
// agent process fail (an out-of-worktree edit is rejected by the fake), so
// every reviewer family errors and the fail-closed panel fails the step.
func reviewerPanelFailScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "panel-fail-scenario.yaml")
	content := fmt.Sprintf(`actions:
  - match: "Review the code changes and return structured findings with a risk assessment."
    text: "panel reviewer error"
    edits:
      - path: "/outside-workdir"
        new: "should fail"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      title: "feat: fakeagent change"
      body: "## Summary\nfakeagent canned PR body"
`)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write panel fail scenario: %v", err)
	}
	return path
}
