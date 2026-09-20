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

func configurableFixCommitScenario(t *testing.T, fixSummary string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "configurable-fix-commit-scenario.yaml")
	content := strings.Replace(`actions:
  - match: "Investigate previous review findings"
    text: "fixed unsafe value"
    edits:
      - path: "feature.txt"
        old: "unsafe"
        new: "safe"
    structured:
      summary: "FIX_SUMMARY"
  - match: "Review the code changes and return structured findings"
    text: "review found an issue"
    structured:
      findings:
        - id: "configurable-fix-1"
          severity: warning
          file: "feature.txt"
          line: 1
          description: "unsafe value needs validation"
          action: auto-fix
      summary: "found one issue"
      risk_level: medium
      risk_rationale: "the unsafe value needs a guard"
      risk_scope: source-or-external
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no remaining risk"
      risk_scope: source-or-external
      tested: ["fakeagent: focused verification"]
      testing_summary: "simulated tests passed"
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      artifacts: []
      title: "fix: guard unsafe value"
      body: "configurable fix commit journey"
`, "FIX_SUMMARY", fixSummary, 1)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write configurable fix commit scenario: %v", err)
	}
	return path
}

func TestConfigurableFixCommitMessageJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: configurableFixCommitScenario(t, "guard unsafe value")})

	globalConfig := filepath.Join(h.NMHome, "config.yaml")
	globalData, err := os.ReadFile(globalConfig)
	if err != nil {
		t.Fatalf("read global config: %v", err)
	}
	globalSource := strings.Replace(string(globalData), "  review: 0\n", "  review: 1\n", 1)
	globalSource += "commit:\n  fix_message: 'chore(global-{{.Step}}): {{.Summary}}'\n"
	if err := os.WriteFile(globalConfig, []byte(globalSource), 0o644); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	const branch = "feature/configurable-fix-commit"
	h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	h.CommitChange(branch, ".no-mistakes.yaml", `ignore_patterns:
  - '*.generated.go'
  - 'vendor/**'
allow_repo_commands: true
commit:
  fix_message: 'fix(repo-{{.Step}}): {{.Summary}}'
`, "configure pipeline fix commits")
	h.PushToGate(branch)

	gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 60*time.Second)
	h.Respond(gated.ID, types.StepReview, types.ActionApprove)
	run := h.WaitForRun(branch, 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, run.Error)
	}

	log, err := h.runGit(context.Background(), h.UpstreamDir, "log", "--format=%s", "main..refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read upstream commit subjects: %v\n%s", err, log)
	}
	subjects := strings.Split(strings.TrimSpace(string(log)), "\n")
	const want = "fix(repo-review): guard unsafe value"
	if len(subjects) == 0 || subjects[0] != want {
		t.Fatalf("latest upstream commit subject = %q, want %q (all subjects: %q)", subjects[0], want, subjects)
	}
	for _, subject := range subjects {
		if strings.HasPrefix(subject, "chore(global-") {
			t.Fatalf("global template won over repository template: %q", subject)
		}
	}

	t.Logf("global config: commit.fix_message = %q", "chore(global-{{.Step}}): {{.Summary}}")
	t.Logf("repository config (higher precedence): commit.fix_message = %q", "fix(repo-{{.Step}}): {{.Summary}}")
	t.Logf("pipeline status: %s", run.Status)
	t.Logf("completed pipeline upstream commit subjects:\n%s", strings.TrimSpace(string(log)))
}

func TestGlobalBranchReplacementFixCommitJourney(t *testing.T) {
	const summary = "Preserve legacy drafts and batch status invariants"
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: configurableFixCommitScenario(t, summary)})

	globalConfig := filepath.Join(h.NMHome, "config.yaml")
	globalData, err := os.ReadFile(globalConfig)
	if err != nil {
		t.Fatalf("read global config: %v", err)
	}
	globalSource := strings.Replace(string(globalData), "  review: 0\n", "  review: 1\n", 1)
	globalSource += `commit:
  branch_pattern: '^PROJ/([0-9]+)$'
  branch_replacement: 'PROJ-${1}'
  fix_message: '{{.Branch}}: {{.Summary}}'
`
	if err := os.WriteFile(globalConfig, []byte(globalSource), 0o644); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	const branch = "PROJ/123"
	h.CommitChange(branch, ".no-mistakes.yaml", `ignore_patterns:
  - '*.generated.go'
  - 'vendor/**'
allow_repo_commands: true
commit:
  branch_replacement: 'WRONG-${1}'
`, "attempt repository branch replacement")
	h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	h.PushToGate(branch)

	gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 60*time.Second)
	h.Respond(gated.ID, types.StepReview, types.ActionApprove)
	run := h.WaitForRun(branch, 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, run.Error)
	}

	log, err := h.runGit(context.Background(), h.UpstreamDir, "log", "--format=%s", "main..refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read upstream commit subjects: %v\n%s", err, log)
	}
	subjects := strings.Split(strings.TrimSpace(string(log)), "\n")
	const want = "PROJ-123: Preserve legacy drafts and batch status invariants"
	if len(subjects) == 0 || subjects[0] != want {
		t.Fatalf("latest upstream commit subject = %q, want %q (all subjects: %q)", subjects[0], want, subjects)
	}

	t.Logf("branch pushed through no-mistakes: %s", branch)
	t.Logf("machine-local replacement: %q", "PROJ-${1}")
	t.Logf("completed pipeline upstream commit subjects:\n%s", strings.TrimSpace(string(log)))
}
