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
// never reuse it. Nothing here enables eval: recording replay provenance is a
// default, so an ordinary run is capturable and replayable as it stands.
func TestEvalJourney(t *testing.T) {
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
	if !strings.Contains(out, "LOCAL-ONLY EVAL REPORT") || !strings.Contains(out, "claude+claude-opus-4-7") || !strings.Contains(out, "unlabeled / pending") || !strings.Contains(out, "queued unmatched candidate findings: 1") {
		t.Fatalf("report output = %q", out)
	}
	t.Logf("eval report output:\n%s", out)
}

// TestEvalAutoCaptureJourney is the end-to-end contract for automatic
// collection: a user who never runs an eval command still ends up with a
// corpus. It deliberately never invokes "eval capture" - the only commands here
// are an ordinary pipeline run and a read-only inspection of the sets.
func TestEvalAutoCaptureJourney(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "auto-capture-scenario.yaml")
	if err := os.WriteFile(scenario, []byte(`actions:
  - match: "Review the code changes and return structured findings with a risk assessment."
    structured:
      findings:
        - id: review-warning
          severity: warning
          file: autocapture.go
          line: 3
          description: "auto-capture scenario finding"
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

	h.CommitChange("auto-capture", "autocapture.go", "package e2e\n\nfunc AutoCapture() {}\n", "add auto-capture change")
	h.PushToGate("auto-capture")
	gated := waitForStepStatus(t, h, "auto-capture", types.StepReview, types.StepStatusAwaitingApproval, 45*time.Second)
	h.Respond(gated.ID, types.StepReview, types.ActionApprove)
	h.WaitForRun("auto-capture", 45*time.Second)

	// Collection runs after the pipeline reports its outcome, so the run being
	// finished is not yet proof the case exists.
	var out string
	deadline := time.Now().Add(30 * time.Second)
	for {
		var err error
		out, err = h.Run("eval", "sets")
		if err != nil {
			t.Fatalf("eval sets: %v\n%s", err, out)
		}
		if strings.Contains(out, "all: 1 cases") || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !strings.Contains(out, "all: 1 cases") {
		t.Fatalf("no eval case was collected without an explicit capture; sets output = %q", out)
	}
	t.Logf("eval sets output after an ordinary run:\n%s", out)
}
