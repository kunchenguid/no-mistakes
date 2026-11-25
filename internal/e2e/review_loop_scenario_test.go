//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

// reviewTurnMarker identifies the initial-review duty by matching the real
// prompt the running binary issues (not by inspecting source).
const reviewTurnMarker = "Review the code changes and return structured findings"

// writeReviewAgentsRoutingScenario scripts the fake agent for a one-finding
// review loop: the initial review reports a single blocking finding (forcing a
// fix round when the operator responds "fix"), the fixer turn resolves it, and
// the rereview comes back clean. Every other step gets the clean catch-all so
// push completes. The role-routing tests that originally owned this scenario
// were removed with the omp-only rebuild; the verification-plan journeys still
// drive this loop.
func writeReviewAgentsRoutingScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "review-agents-routing-scenario.yaml")
	content := `actions:
  - match: "Previous review findings to address"
    text: "addressed the review finding"
    edits:
      - path: "feature.txt"
        old: "seed for review-agents routing\n"
        new: "fixed for review-agents routing\n"
    stage: ["feature.txt"]
    structured:
      summary: "resolve review finding"
  - match: "Previous rounds for this step"
    text: "clean after fix"
    structured:
      findings: []
      summary: "clean after fix"
      risk_level: low
      risk_rationale: "issue resolved by the fixer"
      risk_scope: source-or-external
      reviewed_paths:
        - "feature.txt"
  - match: "` + reviewTurnMarker + `"
    text: "found one blocking issue"
    structured:
      findings:
        - id: "routing-check"
          severity: warning
          description: "mechanical issue routed through the fixer role"
          file: "feature.txt"
          action: auto-fix
          review_scope: source
      summary: "one blocking issue"
      risk_level: low
      risk_rationale: "single mechanical issue"
      risk_scope: source-or-external
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      risk_scope: source-or-external
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      artifacts: []
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      title: "feat: review-agents routing"
      body: "## Summary\nreview-agents routing e2e"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write scenario: %v", err)
	}
	return path
}
