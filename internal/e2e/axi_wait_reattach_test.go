//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// Exercise the CLI and daemon, with agent turns supplied by a scripted
// agent. The independent oracle is the requested hold: an expired call must
// recommend the same duration without replaying an accepted response.
func TestAxiCustomWaitSurvivesReattachmentAndAcceptedResponse(t *testing.T) {
	scenario := writeScenario(t, "slow-review.yaml", `actions:
  - match: "Review the code changes and return structured findings"
    delay_ms: 12000
    text: "one decision for the operator"
    structured:
      findings:
        - id: "default-choice"
          severity: warning
          file: "feature.txt"
          line: 1
          description: "Confirm the feature default before continuing"
          action: ask-user
      summary: "one decision"
      risk_level: medium
      risk_rationale: "the operator must confirm the default"
      risk_scope: source-or-external
`)
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	pushMainRepoConfig(t, h, "allow_repo_commands: true\ncommands:\n  test: sleep 15\n")
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	branch := "feature/custom-wait"
	h.CommitChange(branch, "feature.txt", "flag = true\n", "add feature default")

	assertElapsed := func(args ...string) {
		t.Helper()
		started := time.Now()
		out, err := h.Run(args...)
		t.Logf("no-mistakes %s (elapsed %s):\n%s", strings.Join(args, " "), time.Since(started), out)
		if err == nil || !strings.Contains(out, "wait of 3s elapsed while driving the run") {
			t.Fatalf("expected bounded hold expiry: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Re-run `no-mistakes axi run --wait 3s`") {
			t.Fatalf("reattach lost the hold or replayed the response:\n%s", out)
		}
		if strings.Contains(out, "Re-run `no-mistakes axi respond") || strings.Contains(out, "outcome: failed") {
			t.Fatalf("expiry replayed a decision or failed the pipeline:\n%s", out)
		}
	}

	assertElapsed("axi", "run", "--intent", "confirm the feature default", "--wait", "3s")
	assertElapsed("axi", "run", "--wait", "3s")

	// A six-hour hold must remain attached until a real decision arrives. The
	// slow review also rejects an implementation that treats hours as seconds.
	started := time.Now()
	gate, err := h.Run("axi", "run", "--wait", "6h")
	t.Logf("no-mistakes axi run --wait 6h (elapsed %s):\n%s", time.Since(started), gate)
	if err != nil || !strings.Contains(gate, "default-choice") {
		t.Fatalf("long hold did not reach the decision: %v\n%s", err, gate)
	}
	if time.Since(started) < time.Second {
		t.Fatal("long hold returned before the slow review could finish")
	}
	before := h.Runs()
	if len(before) != 1 || before[0].Branch != branch {
		t.Fatalf("reattachment created a replacement run: %+v", before)
	}

	assertElapsed("axi", "respond", "--action", "approve", "--wait", "3s")
	assertElapsed("axi", "run", "--wait", "3s")
	final, err := h.Run("axi", "run", "--wait", "6h")
	t.Logf("no-mistakes axi run --wait 6h after accepted response:\n%s", final)
	if err != nil || !strings.Contains(final, "outcome: passed") {
		t.Fatalf("reattached run did not finish: %v\n%s", err, final)
	}
	after := h.Runs()
	if len(after) != 1 || after[0].ID != before[0].ID {
		t.Fatalf("response or reattachment replaced the original run: before=%+v after=%+v", before, after)
	}
}
