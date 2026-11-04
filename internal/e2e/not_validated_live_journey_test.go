//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestNotValidatedLiveJourney drives one real CLI/daemon/gate run that leaves
// three kinds of work unvalidated live: a Test scenario the canned evidence
// agent reports as untested, an inconclusive verdict the operator approves with
// a reason (a Test exception), and steps skipped with --skip. It checks that the
// agent-facing run view lists all of them with their reasons, and that the PR
// body published to a stateful fake GitHub carries the Not validated live
// callout even under pr.appendix: minimal, which omits the Testing section.
// The canned agent stands in for the model; nothing here claims it drove a
// product.
func TestNotValidatedLiveJourney(t *testing.T) {
	const (
		branch        = "feature/not-validated-live"
		untestedName  = "refund reaches the payment provider"
		untestedWhy   = "needs a live payment-provider sandbox credential this lab does not have"
		approveReason = "provider sandbox is down; refund path reviewed by hand"
	)
	scenario := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(scenario, []byte(`actions:
  - match: "You are validating a code change by driving the product itself."
    text: "two of three scenarios driven"
    structured:
      findings: []
      summary: "two of three scenarios driven"
      tested: ["fakeagent: drove checkout and cart"]
      testing_summary: "drove checkout and cart; refund could not be driven"
      artifacts: []
      verdict: inconclusive
      scenarios:
        - name: checkout succeeds
          result: pass
          live: true
          evidence: "fakeagent: checkout returned 200"
          reason: ""
        - name: cart persists
          result: pass
          live: true
          evidence: "fakeagent: cart survived a reload"
          reason: ""
        - name: `+untestedName+`
          result: untested
          live: false
          evidence: ""
          reason: "`+untestedWhy+`"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "synthetic clean response"
      risk_scope: source-or-external
`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	statePath := setupStatefulGitHub(t, h)
	// pr.appendix is trusted default-branch config.
	h.CommitChange("main", ".no-mistakes.yaml", "allow_repo_commands: true\nno_ci: true\npr:\n  appendix: minimal\n", "configure minimal appendix")
	if out, err := h.runGit(t.Context(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push main: %v\n%s", err, out)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange(branch, "refund.txt", "refund feature\n", "add refunds")
	if out, err := h.Run("axi", "run", "--intent", "Let a customer refund an order", "--skip", "document,ci"); err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusAwaitingApproval, 60*time.Second)
	respond, err := h.Run("axi", "respond", "--step", "test", "--action", "approve", "--reason", approveReason)
	if err != nil {
		t.Fatalf("respond: %v\n%s", err, respond)
	}
	t.Logf("axi respond output:\n%s", respond)
	assertValidationBlock(t, "respond", respond, untestedName, untestedWhy, approveReason, true)

	status, err := h.Run("axi", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, status)
	}
	t.Logf("axi status output:\n%s", status)
	assertValidationBlock(t, "status", status, untestedName, untestedWhy, approveReason, false)

	body := livePRBody(t, statePath, branch)
	t.Logf("published PR body:\n%s", body)
	callout := strings.Index(body, "## Not validated live")
	if callout < 0 {
		t.Fatalf("published body has no Not validated live callout:\n%s", body)
	}
	for _, want := range []string{
		"1 of 3 scenarios were not driven live against the product.",
		"**Untested:** " + untestedName + " - " + untestedWhy,
		"**Test exception** (`test`): live validation verdict: inconclusive Test exception approved: " + approveReason,
		"**Skipped by operator** (`document`): no reason recorded",
		"**Skipped by operator** (`ci`): no reason recorded",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("published body missing %q", want)
		}
	}
	if strings.Contains(body, "## Testing") {
		t.Errorf("minimal appendix published the Testing section")
	}
	if changed := strings.Index(body, "## What Changed"); changed >= 0 && changed < callout {
		t.Errorf("callout does not lead the narrative")
	}
	if fold := strings.Index(body, "<details"); fold >= 0 && fold < callout {
		t.Errorf("callout is inside a fold")
	}
}

func assertValidationBlock(t *testing.T, label, out, untestedName, untestedWhy, approveReason string, wantHelp bool) {
	t.Helper()
	start := strings.Index(out, "run:\n")
	if start < 0 {
		t.Fatalf("%s: no run document:\n%s", label, out)
	}
	var doc struct {
		Outcome string `toon:"outcome"`
		Run     struct {
			Validation struct {
				Verdict  string `toon:"verdict"`
				Live     int    `toon:"live"`
				Total    int    `toon:"total"`
				Untested []struct {
					Scenario string `toon:"scenario"`
					Reason   string `toon:"reason"`
				} `toon:"untested"`
				Exceptions []struct {
					Step   string `toon:"step"`
					Kind   string `toon:"kind"`
					Reason string `toon:"reason"`
				} `toon:"exceptions"`
			} `toon:"validation"`
		} `toon:"run"`
		Help []string `toon:"help"`
	}
	if err := toon.UnmarshalString(out[start:], &doc); err != nil {
		t.Fatalf("%s: parse output: %v\n%s", label, err, out)
	}
	if doc.Outcome != "passed-with-override" {
		t.Fatalf("%s: outcome = %q, want passed-with-override", label, doc.Outcome)
	}
	v := doc.Run.Validation
	if v.Verdict != types.TestVerdictInconclusive || v.Live != 2 || v.Total != 3 {
		t.Fatalf("%s: validation = %+v", label, v)
	}
	if len(v.Untested) != 1 || v.Untested[0].Scenario != untestedName || v.Untested[0].Reason != untestedWhy {
		t.Fatalf("%s: untested = %+v", label, v.Untested)
	}
	kinds := map[string]string{}
	for _, e := range v.Exceptions {
		kinds[e.Step+"/"+e.Kind] = e.Reason
	}
	if !strings.Contains(kinds["test/test-exception"], approveReason) {
		t.Fatalf("%s: Test exception reason missing: %+v", label, v.Exceptions)
	}
	for _, step := range []string{"document", "ci"} {
		if kinds[step+"/operator-skip"] != "no reason recorded" {
			t.Fatalf("%s: %s operator skip missing: %+v", label, step, v.Exceptions)
		}
	}
	if !wantHelp {
		return
	}
	joined := strings.Join(doc.Help, "\n")
	for _, want := range []string{"1 of 3 scenarios were not validated live", "The document step was skipped by the operator", "Report the approved Test exception"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("%s: help missing %q:\n%s", label, want, joined)
		}
	}
}
