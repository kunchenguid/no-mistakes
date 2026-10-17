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

// A fix-review gate must stop replaying a finding an earlier round already
// fixed once the rereview's only same-file report is a distinct defect far
// from it, while a report close enough to be that same defect shifted or
// reworded by its fix still keeps the finding outstanding.
func TestFixReviewGateDropsVerifiedFindingDespiteDistantSameFileReport(t *testing.T) {
	for _, tc := range []struct {
		name      string
		line      int
		staysOpen bool
	}{
		{name: "distant distinct defect clears the fixed finding", line: 200},
		{name: "nearby restatement keeps the fixed finding", line: 14, staysOpen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scenario := filepath.Join(t.TempDir(), "agent.yaml")
			body := fmt.Sprintf(`actions:
  - match: "Fix-round provenance:"
    structured:
      findings:
        - id: other-defect
          severity: warning
          file: service.txt
          line: %d
          description: Unchecked error return in the helper
          action: ask-user
      summary: "verification round"
      risk_level: medium
      risk_rationale: "one remaining concern"
      risk_scope: source-or-external
  - match: "Investigate previous review findings"
    edits:
      - path: service.txt
        new: "corrected service\n"
    structured:
      findings: []
      summary: "fix applied"
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: nil-deref
          severity: error
          file: service.txt
          line: 10
          description: Nil dereference on the service handle
          action: ask-user
      summary: "one concern"
      risk_level: medium
      risk_rationale: "nil dereference"
      risk_scope: source-or-external
  - structured:
      findings: []
      summary: "clean"
      risk_level: low
      risk_rationale: "none"
      risk_scope: source-or-external
      tested: ["fixture"]
      testing_summary: "fixture"
      scenarios:
        - name: "fixture"
          result: pass
          live: true
          evidence: "fixture"
          reason: ""
      verdict: go
      artifacts: []
      title: "fix: service"
      body: "Service fixed"
`, tc.line)
			if err := os.WriteFile(scenario, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}
			branch := "feature/review-line-window"
			h.CommitChange(branch, "service.txt", "broken service\n", "service")
			initial, err := h.Run("axi", "run", "--intent", "Fix the selected review finding")
			if err != nil || !strings.Contains(initial, "nil-deref") {
				t.Fatalf("initial review gate: %v\n%s", err, initial)
			}
			response, err := h.Run("axi", "respond", "--action", "fix", "--findings", "nil-deref")
			if err != nil {
				t.Fatalf("select fix: %v\n%s", err, response)
			}

			run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 90*time.Second)
			status, err := h.Run("axi", "status")
			if err != nil {
				t.Fatalf("axi status: %v\n%s", err, status)
			}
			if !strings.Contains(status, "Unchecked error return in the helper") {
				t.Fatalf("the rereview's own finding is missing from the gate; run=%s status:\n%s", run.ID, status)
			}
			replayed := strings.Contains(status, "Nil dereference on the service handle")
			if replayed != tc.staysOpen {
				if tc.staysOpen {
					t.Fatalf("the fixed finding's nearby restatement cleared it; run=%s status:\n%s", run.ID, status)
				}
				t.Fatalf("the fix-review gate replayed a finding verified by this round; run=%s status:\n%s", run.ID, status)
			}
			t.Logf("fix-review gate; run=%s status:\n%s", run.ID, status)
		})
	}
}
