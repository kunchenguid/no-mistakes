//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This is a real CLI/daemon/gate journey with a local bare push target and a
// canned evidence agent, not a claim that the canned agent tested a product.
func TestAxiTestApprovalExceptionJourney(t *testing.T) {
	for _, tc := range []struct {
		name, action, reason string
		exit                 int
		verdict              string
		qualified            bool
	}{
		{name: "clean"},
		{name: "command-reason", exit: 7, action: "approve", reason: "Synthetic exception: accept exit 7 only for this isolated test", qualified: true},
		{name: "command-no-reason", exit: 7, action: "approve", qualified: true},
		{name: "evidence-exception", verdict: "no-go", action: "approve", reason: "Synthetic failed scenario accepted for this test", qualified: true},
		{name: "evidence-inconclusive", verdict: "inconclusive", action: "approve", reason: "Synthetic inconclusive evidence accepted", qualified: true},
		// A no-surface acknowledgement keeps its reason but completes normally.
		{name: "no-surface-ack", verdict: "no-surface", action: "approve", reason: "Docs-only change acknowledged"},
		{name: "skipped", exit: 7, action: "skip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scenario := ""
			if tc.verdict != "" {
				scenario = filepath.Join(t.TempDir(), "scenario.yaml")
				scenarioYAML := `        - name: synthetic scenario
          result: fail
          live: true
          evidence: "fakeagent: simulated failure"
          reason: ""`
				switch tc.verdict {
				case "inconclusive":
					scenarioYAML = `        - name: synthetic scenario
          result: untested
          live: false
          evidence: "fakeagent: not driven"
          reason: "synthetic missing capability"`
				case "no-surface":
					scenarioYAML = `        - name: synthetic docs scenario
          result: untested
          live: false
          evidence: "fakeagent: no runtime surface"
          reason: "synthetic docs-only change"`
				}
				// Only Test receives the scenario verdict. Other agent phases
				// complete without findings, exactly as in the clean control.
				data := `actions:
  - match: "You are validating a code change by driving the product itself."
    text: "synthetic scenario"
    structured:
      findings: []
      summary: "synthetic scenario"
      tested: ["fakeagent: simulated scenario"]
      testing_summary: "synthetic scenario"
      artifacts: []
      verdict: ` + tc.verdict + `
      scenarios:
` + scenarioYAML + `
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "synthetic clean response"
      risk_scope: source-or-external
`
				if err := os.WriteFile(scenario, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}
			h.CommitChange(tc.name, ".no-mistakes.yaml", fmt.Sprintf("commands:\n  test: 'exit %d'\n  lint: 'exit 0'\n", tc.exit), "configure synthetic test")
			head := h.CommitChange(tc.name, "feature.txt", "synthetic feature\n", "add feature")
			out, err := h.Run("axi", "run", "--intent", "Validate the synthetic feature", "--skip", "pr,ci")
			if err != nil {
				t.Fatalf("run: %v\n%s", err, out)
			}
			if tc.action != "" {
				gated := waitForStepStatus(t, h, tc.name, types.StepTest, types.StepStatusAwaitingApproval, 30*time.Second)
				if gated.HeadSHA != head || gated.Branch != tc.name {
					t.Fatalf("wrong gate identity: %+v", gated)
				}
				args := []string{"axi", "respond", "--step", "test", "--action", tc.action}
				if tc.reason != "" {
					args = append(args, "--reason", tc.reason)
				}
				out, err = h.Run(args...)
				if err != nil {
					t.Fatalf("respond: %v\n%s", err, out)
				}
			}
			want := "passed"
			if tc.qualified {
				want = "passed-with-override"
			}
			assertTestExceptionOutput(t, out, want, tc.reason)
			status, err := h.Run("axi", "status")
			if err != nil {
				t.Fatalf("status: %v\n%s", err, status)
			}
			assertTestExceptionOutput(t, status, want, tc.reason)

			// An independent read-only connection proves the evidence is durable,
			// not just cached in the drive command or on an IPC event.
			database, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			run := h.WaitForRun(tc.name, 30*time.Second)
			steps, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range steps {
				if step.StepName != types.StepTest {
					continue
				}
				if step.ExitCode == nil || *step.ExitCode != tc.exit {
					t.Fatalf("underlying exit code lost: %+v", step)
				}
				if tc.action == "approve" {
					if step.ApprovalReason == nil || *step.ApprovalReason != tc.reason {
						t.Fatalf("operator reason lost: %+v", step)
					}
					if step.FindingsJSON == nil || !strings.Contains(*step.FindingsJSON, "test-1") {
						t.Fatalf("underlying finding lost: %+v", step)
					}
				} else if step.ApprovalReason != nil {
					t.Fatalf("ordinary completion or skip mislabeled as approval: %+v", step)
				}
				// Only a configured-command failure receives the existing marker
				// consumed by PR enforcement. Evidence approvals cannot change it.
				if (step.OverrideReason != nil) != (tc.action == "approve" && tc.exit != 0) {
					t.Fatalf("command waiver policy changed: %+v", step)
				}
			}
		})
	}
}

func assertTestExceptionOutput(t *testing.T, out, want, reason string) {
	t.Helper()
	// Progress is stderr; the TOON document begins at the structured run object.
	start := strings.Index(out, "run:\n")
	if start < 0 {
		t.Fatalf("no run document:\n%s", out)
	}
	var doc struct {
		Outcome string `toon:"outcome"`
		Run     struct {
			Reason string `toon:"test_override_reason"`
		} `toon:"run"`
	}
	if err := toon.UnmarshalString(out[start:], &doc); err != nil {
		t.Fatalf("parse output: %v\n%s", err, out)
	}
	if doc.Outcome != want {
		t.Fatalf("outcome = %q, want %q\n%s", doc.Outcome, want, out)
	}
	if want == "passed-with-override" {
		if doc.Run.Reason == "" || (reason != "" && !strings.Contains(doc.Run.Reason, reason)) {
			t.Fatalf("exception evidence missing:\n%s", out)
		}
	} else if doc.Run.Reason != "" {
		t.Fatalf("unapproved run reported exception: %+v", doc)
	}
}
