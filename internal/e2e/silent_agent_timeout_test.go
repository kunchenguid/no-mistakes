//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// silentAgentScenario makes the review agent go completely silent: it produces
// no stdout, no stderr, and never responds, exactly like the native agent that
// burned two 30-minute budgets on one task without emitting a single byte. The
// hang is far longer than the budget the test configures, so the pipeline - not
// the fake - is what ends the invocation.
func silentAgentScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "silent-agent-scenario.yaml")
	content := `actions:
  - match: "Review the code changes and return structured findings"
    delay_ms: 60000
    text: "never reached"
    structured:
      findings: []
      summary: "never reached"
      risk_level: low
      risk_rationale: "never reached"
      risk_scope: source-or-external
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      risk_scope: source-or-external
      tested: ["fakeagent: simulated test run"]
      testing_summary: "simulated tests passed"
      artifacts: []
      title: "feat: fakeagent change"
      body: "## Summary\nfakeagent canned PR body"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write silent agent scenario: %v", err)
	}
	return path
}

// TestSilentAgentTimeoutReportsMeasuredEvidence reproduces the end-user failure
// through the stock `no-mistakes axi` surface: a pipeline agent that starts and
// then emits nothing at all until its invocation budget expires.
//
// What the operator used to get was "agent timed out after 30m0s (agent silent
// for 30m0s)" - the configured budget printed twice, with whatever the adapter
// reported discarded. Nothing in that line was measured, so a wedged agent, a
// busy one, and a crashed one were indistinguishable, and two silent 30-minute
// timeouts on one task produced no evidence to act on.
//
// The budgets here are seconds rather than the production 30 minutes; the
// timeout path is identical, and the test must never be "fixed" by raising them.
func TestSilentAgentTimeoutReportsMeasuredEvidence(t *testing.T) {
	h := NewHarness(t, SetupOpts{
		Agent:    "claude",
		Scenario: silentAgentScenario(t),
		GlobalConfigExtra: strings.Join([]string{
			`agent_timeout: "3s"`,
			`review_agent_timeout: "3s"`,
		}, "\n"),
	})

	h.CommitChange("init-silent", "seed.txt", "seed\n", "seed silent init")
	initWorktree := h.AddWorktree("init-silent")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	h.CommitChange("feature/silent-agent", "feature.txt", "value\n", "add feature")
	operator := h.AddWorktree("feature/silent-agent")

	runOut, runErr := h.RunInDir(operator, "axi", "run", "--intent", "validate the feature while the agent is wedged")
	if runErr == nil {
		t.Fatalf("expected the wedged review agent to fail the run:\n%s", runOut)
	}

	statusOut, _ := h.RunInDir(operator, "axi", "status")
	// Keep the real operator surfaces visible under `go test -v` so this
	// regression can also produce reviewer-visible evidence of the executable
	// journey rather than only a pass/fail result.
	t.Logf("stock axi run surface:\n%s", runOut)
	t.Logf("stock axi status surface:\n%s", statusOut)
	surfaces := runOut + "\n" + statusOut

	// The budget that expired must be named...
	if !strings.Contains(surfaces, "timed out after 3s") {
		t.Fatalf("axi surfaces did not name the expired budget:\n--- run ---\n%s\n--- status ---\n%s", runOut, statusOut)
	}
	// ...and the silence must be a measurement, not the budget restated.
	if !strings.Contains(surfaces, "produced no output at all") {
		t.Fatalf("axi surfaces did not report measured silence:\n--- run ---\n%s\n--- status ---\n%s", runOut, statusOut)
	}
	if strings.Contains(surfaces, "silent for 3s") {
		t.Fatalf("axi surfaces restated the budget as if it were a measurement:\n--- run ---\n%s\n--- status ---\n%s", runOut, statusOut)
	}
	// The launched-but-mute subprocess is named, which is the fact that
	// separates "the agent never started" from "the agent started and wedged".
	if !strings.Contains(surfaces, "after its subprocess started") {
		t.Fatalf("axi surfaces did not distinguish a launched agent from one that never ran:\n--- run ---\n%s\n--- status ---\n%s", runOut, statusOut)
	}
	// The adapter's own account of the killed process survives to the operator.
	if !strings.Contains(surfaces, "agent reported:") {
		t.Fatalf("axi surfaces discarded the adapter's report:\n--- run ---\n%s\n--- status ---\n%s", runOut, statusOut)
	}
}
