//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestEvalJourney drives the public CLI through a real captured pipeline run
// and replays its review with a fakeagent scenario. The harness's NM_HOME owns
// the source daemon; eval itself must create its own temporary sandbox and
// never reuse it.
func TestEvalJourney(t *testing.T) {
	t.Setenv("NO_MISTAKES_EVAL_CAPTURE_PROVENANCE", "1")
	scenario := filepath.Join(t.TempDir(), "eval-scenario.yaml")
	if err := os.WriteFile(scenario, []byte(`actions:
  - match: "Review the code changes and return structured findings with a risk assessment."
    structured:
      findings:
        - id: review-warning
          severity: warning
          file: eval.go
          line: 3
          description: "review scenario finding"
          action: ask-user
          review_scope: source
      risk_level: medium
      risk_rationale: "scenario review finding"
      risk_scope: source-or-external
  - structured:
      findings: []
      summary: "clean"
      tested: ["fakeagent"]
      testing_summary: "simulated"
      artifacts: []
      risk_level: low
      risk_rationale: "clean"
      risk_scope: source-or-external
      title: "fake"
      body: "fake"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	h.CommitChange("eval-journey", "eval.go", "package e2e\n\nfunc EvalJourney() {}\n", "add eval journey change")
	h.PushToGate("eval-journey")
	gated := waitForStepStatus(t, h, "eval-journey", types.StepReview, types.StepStatusAwaitingApproval, 45*time.Second)
	h.Respond(gated.ID, types.StepReview, types.ActionApprove)
	run := h.WaitForRun("eval-journey", 45*time.Second)

	out, err := h.Run("eval", "capture", run.ID)
	if err != nil {
		t.Fatalf("eval capture: %v\n%s", err, out)
	}
	if !strings.Contains(out, "captured 1 local review case") {
		t.Fatalf("capture output = %q", out)
	}
	t.Logf("eval capture output:\n%s", out)

	out, err = h.Run("eval", "sets")
	if err != nil {
		t.Fatalf("eval sets: %v\n%s", err, out)
	}
	if !strings.Contains(out, "LOCAL-ONLY EVAL CASE SETS") || !strings.Contains(out, "diversified:") {
		t.Fatalf("sets output = %q", out)
	}
	t.Logf("eval sets output:\n%s", out)

	out, err = h.Run("eval", "run", "--cases", "all", "--candidate", "claude+claude-opus-4-7", "--repeats", "1")
	if err != nil {
		report, reportErr := h.Run("eval", "report")
		t.Fatalf("eval run: %v\n%s\neval report after failure (%v):\n%s", err, out, reportErr, report)
	}
	if !strings.Contains(out, "local eval session") {
		t.Fatalf("run output = %q", out)
	}
	t.Logf("eval run output:\n%s", out)
	invocations := h.AgentInvocations()
	if len(invocations) == 0 {
		t.Fatal("expected replay agent invocation")
	}
	replayCWD := invocations[len(invocations)-1].CWD
	if !strings.Contains(replayCWD, "nm-eval-replay-") || strings.HasPrefix(replayCWD, h.NMHome) {
		t.Fatalf("replay used non-isolated worktree %q (source NM_HOME %q)", replayCWD, h.NMHome)
	}

	out, err = h.Run("eval", "report")
	if err != nil {
		t.Fatalf("eval report: %v\n%s", err, out)
	}
	if !strings.Contains(out, "LOCAL-ONLY EVAL REPORT") || !strings.Contains(out, "claude+claude-opus-4-7") || !strings.Contains(out, "queued unexpected parks: 1") {
		t.Fatalf("report output = %q", out)
	}
	t.Logf("eval report output:\n%s", out)
}
