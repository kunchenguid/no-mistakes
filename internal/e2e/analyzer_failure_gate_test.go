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

// TestAnalyzerEvidenceFailuresFailPipelineJourney exercises the real gate
// transport, native-agent adapter, and persisted run state. A native response
// that cannot certify a pipeline phase must terminate the run instead of
// producing an approval that AXI --yes could accept.
func TestAnalyzerEvidenceFailuresFailPipelineJourney(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "analyzer-failure-gate.yaml")
	content := `actions:
  - match: "report only what you could not resolve.\n\nContext:\n- branch: analyzer-document-malformed-output"
    text: "documentation unavailable"
    structured_raw: '{"summary":123}'
  - match: "You are validating a code change by testing it. Examine the repository and run the smallest relevant tests yourself.\n\nContext:\n- branch: analyzer-document-malformed-output"
    text: "tests passed"
    structured:
      findings: []
      summary: "targeted test passed"
      tested:
        - "fakeagent: targeted test"
      testing_summary: "targeted validation passed"
      artifacts: []
  - match: "Detect the linting and formatting tools for this project, run the relevant checks yourself, apply safe fixes, and verify the result.\n\nContext:\n- branch: analyzer-lint-malformed-output"
    text: "lint unavailable"
    structured_raw: '{"summary":123}'
  - match: "You are validating a code change by testing it. Examine the repository and run the smallest relevant tests yourself.\n\nContext:\n- branch: analyzer-lint-malformed-output"
    text: "tests passed"
    structured:
      findings: []
      summary: "targeted test passed"
      tested:
        - "fakeagent: targeted test"
      testing_summary: "targeted validation passed"
      artifacts: []
  - match: "branch: analyzer-review-null-findings"
    text: "review unavailable"
    structured_raw: '{"findings":null,"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}'
  - match: "Review the code changes and return structured findings"
    text: "review clean"
    structured:
      findings: []
      risk_level: low
      risk_rationale: "no source risks"
      risk_scope: source-or-external
  - match: "You are validating a code change by testing it."
    text: "tests unavailable"
    structured_raw: '{"findings":[],"summary":""}'
`
	if err := os.WriteFile(scenario, []byte(content), 0o644); err != nil {
		t.Fatalf("write scenario: %v", err)
	}

	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	h.CommitChange("main", ".no-mistakes.yaml", "ignore_patterns:\n  - '*.generated.go'\n", "ignore generated test fixture")
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push trusted test config: %v\n%s", err, out)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	for _, tc := range []struct {
		branch     string
		step       types.StepName
		stepError  string
		changePath string
	}{
		{
			branch:     "analyzer-document-malformed-output",
			step:       types.StepDocument,
			stepError:  "validate document analyzer findings",
			changePath: "document.txt",
		},
		{
			branch:     "analyzer-review-null-findings",
			step:       types.StepReview,
			stepError:  "review analyzer findings missing findings array",
			changePath: "review.txt",
		},
		{
			branch:     "analyzer-test-incomplete-evidence",
			step:       types.StepTest,
			stepError:  "validate test analyzer findings",
			changePath: "test.txt",
		},
		{
			branch:     "analyzer-lint-malformed-output",
			step:       types.StepLint,
			stepError:  "validate lint analyzer findings",
			changePath: "lint.generated.go",
		},
	} {
		t.Run(string(tc.step), func(t *testing.T) {
			h.CommitChange(tc.branch, tc.changePath, "change\n", "exercise "+string(tc.step)+" analyzer gate")
			h.PushToGate(tc.branch)
			run := h.WaitForRun(tc.branch, 60*time.Second)
			if run.Status != types.RunFailed {
				t.Fatalf("run status = %s, want failed (error=%v)", run.Status, deref(run.Error))
			}
			step, ok := findStep(run.Steps, tc.step)
			if !ok {
				t.Fatalf("missing %s step", tc.step)
			}
			if step.Status != types.StepStatusFailed {
				t.Fatalf("%s status = %s, want failed", tc.step, step.Status)
			}
			if step.Error == nil || !strings.Contains(*step.Error, tc.stepError) {
				t.Fatalf("%s error = %q, want %q", tc.step, deref(step.Error), tc.stepError)
			}
			t.Logf("persisted run: branch=%s status=%s step=%s step_status=%s error=%s", run.Branch, run.Status, tc.step, step.Status, deref(step.Error))
		})
	}
}
