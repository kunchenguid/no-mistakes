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

// A real CLI fix selection must settle a pre-upgrade, unanchored finding only
// after the reviewer verifies the entire changed-file set without re-reporting it.
func TestFilelessSelectedReviewFindingRequiresCompleteVerification(t *testing.T) {
	for _, tc := range []struct {
		name, coverage, rereport string
		clears                   bool
	}{
		{name: "full coverage clears", clears: true},
		{name: "partial coverage parks", coverage: "      reviewed_paths: [service.txt]"},
		{name: "rereported finding parks", rereport: "        - id: rereported\n          severity: warning\n          description: Unanchored legacy concern\n          action: ask-user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings := "[]"
			if tc.rereport != "" {
				findings = "\n" + tc.rereport
			}
			coverage := ""
			if tc.coverage != "" {
				coverage = tc.coverage + "\n"
			}
			scenario := filepath.Join(t.TempDir(), "agent.yaml")
			body := fmt.Sprintf(`actions:
  - match: "Fix-round provenance:"
    structured:
      findings: %s
      summary: "verification round"
      risk_level: low
      risk_rationale: "checked the changes"
      risk_scope: source-or-external
%s  - match: "Investigate previous review findings"
    edits:
      - path: service.txt
        new: "corrected service\n"
    structured:
      findings: []
      summary: "fix applied"
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: legacy-unanchored
          severity: warning
          description: Unanchored legacy concern
          action: ask-user
      summary: "one concern"
      risk_level: medium
      risk_rationale: "unanchored concern"
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
`, findings, coverage)
			if err := os.WriteFile(scenario, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}
			branch := "feature/fileless-review"
			h.CommitChange(branch, "service.txt", "broken service\n", "service")
			h.CommitChange(branch, "cache.txt", "changed cache\n", "cache")
			initial, err := h.Run("axi", "run", "--intent", "Fix selected unanchored review concern")
			if err != nil || !strings.Contains(initial, "legacy-unanchored") {
				t.Fatalf("initial review gate: %v\n%s", err, initial)
			}
			response, err := h.Run("axi", "respond", "--action", "fix", "--findings", "legacy-unanchored")
			if err != nil {
				t.Fatalf("select fix: %v\n%s", err, response)
			}
			if tc.clears {
				run := h.WaitForRun(branch, 90*time.Second)
				if run.Status != types.RunCompleted {
					t.Fatalf("positive verification did not clear selected finding: status=%s error=%v response=%s", run.Status, run.Error, response)
				}
				status, err := h.Run("axi", "status")
				if err != nil || !strings.Contains(status, "status: completed") || strings.Contains(status, "legacy-unanchored,warning") {
					t.Fatalf("completed run not reflected in CLI status: %v\n%s", err, status)
				}
				t.Logf("selected unanchored finding cleared; run=%s CLI status:\n%s", run.ID, status)
				return
			}
			run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 90*time.Second)
			status, err := h.Run("axi", "status")
			if err != nil || !strings.Contains(status, "Unanchored legacy concern") {
				t.Fatalf("selected finding vanished after incomplete verification: %v\n%s", err, status)
			}
			t.Logf("selected unanchored finding still parked; run=%s status:\n%s", run.ID, status)
		})
	}
}
