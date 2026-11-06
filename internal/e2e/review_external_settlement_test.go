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

// The journey an operator takes when a review finding can only be settled
// outside the review worktree: a secret rotation that lives in a private infra
// repository the reviewer cannot read. The operator records the settlement with
// its reason, the reviewer re-reports the same concern under a new id at a new
// line, and the gate must stop listing it so the remaining coverage can be
// completed - while the step still parks for an explicit verdict instead of
// reporting a clean pass, and the published PR body reports the finding as
// settled without a fix and never carries the operator's private reason.
//
// The oracle is docs/src/content/docs/reference/cli.md (`--action
// resolved_external`) and the Coverage continuation section of
// reference/pipeline-steps.md.
func TestReviewExternalSettlementJourney(t *testing.T) {
	const concern = "the deploy reads a rotation token this worktree cannot see, so the rotation cannot be verified here"
	const privateReason = "rotation applied in the private infra repository, verified by the platform owner"

	scenario := filepath.Join(t.TempDir(), "agent.yaml")
	// Matched in order against the prompt. The focused coverage turn carries
	// the round history too, so its own marker must be tested first.
	body := `actions:
  - match: "Focused coverage completion:"
    structured:
      findings: []
      summary: the remaining file is clean
      risk_level: low
      risk_rationale: nothing else to report in the remaining file
      risk_scope: source-or-external
      reviewed_paths: [snapshot.sh]
  - match: "user_resolved_external"
    structured:
      findings:
        - id: rotation-again
          severity: error
          file: deploy.sh
          line: 31
          description: ` + concern + `
          action: ask-user
          review_scope: source
      summary: the rotation concern is still visible from here
      risk_level: high
      risk_rationale: the deploy still depends on a secret this worktree cannot read
      risk_scope: source-or-external
      reviewed_paths: [deploy.sh]
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: rotation
          severity: error
          file: deploy.sh
          line: 4
          description: ` + concern + `
          action: ask-user
          review_scope: source
      summary: one rotation concern
      risk_level: high
      risk_rationale: the deploy depends on a secret outside this repository
      risk_scope: source-or-external
      reviewed_paths: [deploy.sh]
  - structured:
      findings: []
      summary: clean
      risk_level: low
      risk_rationale: nothing else to report
      risk_scope: source-or-external
      tested: ["fixture"]
      testing_summary: fixture
      scenarios:
        - name: fixture
          result: pass
          live: true
          evidence: fixture
          reason: ""
      verdict: go
      artifacts: []
      title: "feat: rotate the deploy token"
      body: Deploy token rotation
`
	if err := os.WriteFile(scenario, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	statePath := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/rotation-settlement"
	h.CommitChange(branch, "deploy.sh", "#!/usr/bin/env bash\n# deploy using the rotation token\ndeploy --token \"${DEPLOY_TOKEN}\"\n", "add deploy script")
	h.CommitChange(branch, "snapshot.sh", "#!/usr/bin/env bash\n# archive the release\ntar -czf /srv/snapshots/app.tar.gz /srv/app\n", "add snapshot script")

	gate1, err := h.Run("axi", "run", "--intent", "Rotate the deploy token and keep release artifacts out of the repository", "--skip", "ci")
	if err != nil || !strings.Contains(gate1, "rotation") {
		t.Fatalf("initial review gate: %v\n%s", err, gate1)
	}

	// Adversarial: every malformed settlement is refused and the gate stays
	// parked, so none of them can settle anything by accident.
	refusals := []struct {
		name string
		args []string
		want string
	}{
		{"no reason", []string{"axi", "respond", "--action", "resolved_external", "--findings", "rotation"}, "resolved_external requires a Review gate"},
		{"with ignore", []string{"axi", "respond", "--action", "resolved_external", "--findings", "rotation", "--reason", privateReason, "--ignore", "rotation"}, "resolved_external requires a Review gate"},
		{"wrong step", []string{"axi", "respond", "--step", "test", "--action", "resolved_external", "--findings", "rotation", "--reason", privateReason}, "resolved_external requires a Review gate"},
		{"unknown finding", []string{"axi", "respond", "--step", "review", "--action", "resolved_external", "--findings", "not-a-finding", "--reason", privateReason}, "current Review gate"},
	}
	for _, tc := range refusals {
		out, err := h.Run(tc.args...)
		if err == nil {
			t.Fatalf("%s: settlement was accepted; output:\n%s", tc.name, out)
		}
		if !strings.Contains(out, tc.want) {
			t.Fatalf("%s: refusal does not explain the problem (want %q):\n%s", tc.name, tc.want, out)
		}
	}

	// The gate survived every refusal: the real settlement is still accepted.
	gate2, err := h.Run("axi", "respond", "--step", "review", "--action", "resolved_external", "--findings", "rotation", "--reason", privateReason)
	if err != nil {
		t.Fatalf("settle the rotation finding: %v\n%s", err, gate2)
	}

	echo := recordedDispositionsBlock(gate2)
	if echo == "" {
		t.Fatalf("settlement response has no recorded: block:\n%s", gate2)
	}
	external := ""
	for _, line := range strings.Split(echo, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "resolved_external") {
			external = line
			break
		}
	}
	if !strings.Contains(external, "rotation") {
		t.Fatalf("recorded resolved_external line %q does not report the settled finding; echo:\n%s", external, echo)
	}

	// The reviewer re-reported the same concern as rotation-again at line 31.
	// The settlement must keep it off the gate, and the gate must now offer the
	// coverage continuation it was blocking.
	if strings.Contains(gate2, "rotation-again") {
		t.Fatalf("the settled finding was re-reported on the next gate under a new id:\n%s", gate2)
	}
	if !strings.Contains(gate2, "--action complete-coverage") {
		t.Fatalf("the gate does not offer the coverage continuation the settlement unblocked:\n%s", gate2)
	}

	// The documented round export is the durable record of the settlement: the
	// settled round carries the resolved_external source and the operator's
	// reason, and the round it started is an "external" trigger whose payload
	// no longer reports the concern.
	settled := waitForReviewRounds(t, h, branch, 2, 30*time.Second)
	if got := settled.export.Rounds[0].SelectionSource; got != "resolved_external" {
		t.Fatalf("round 1 selection_source = %q, want resolved_external:\n%s", got, settled.raw)
	}
	if got := settled.export.Rounds[1].Trigger; got != "external" {
		t.Fatalf("round 2 trigger = %q, want external:\n%s", got, settled.raw)
	}
	for _, item := range settled.export.Rounds[1].Findings {
		if item.Description == concern {
			t.Fatalf("round 2 still records the settled concern:\n%s", settled.raw)
		}
	}
	if !strings.Contains(settled.raw, privateReason) {
		t.Fatalf("the settled round does not keep the operator's reason:\n%s", settled.raw)
	}

	gate3, err := h.Run("axi", "respond", "--step", "review", "--action", "complete-coverage")
	if err != nil {
		t.Fatalf("complete coverage after the settlement: %v\n%s", err, gate3)
	}

	// Coverage is complete and nothing is reported, yet the step still parks:
	// a run carrying an operator settlement is never a silent clean pass.
	parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	status, err := h.Run("axi", "status")
	if err != nil {
		t.Fatalf("axi status while parked: %v\n%s", err, status)
	}
	if strings.Contains(status, "complete-coverage") {
		t.Fatalf("coverage is complete but the gate still offers a continuation:\n%s", status)
	}
	t.Logf("review parked for an explicit verdict after full coverage; run=%s", parked.ID)

	final, err := h.Run("axi", "respond", "--step", "review", "--action", "approve")
	if err != nil {
		t.Fatalf("approve the settled review: %v\n%s", err, final)
	}
	run := waitPublished(t, h, branch, "", "settlement journey")
	if run.Status != types.RunCompleted && run.Status != types.RunChecksPassed {
		t.Fatalf("run %s ended %s (error=%v)", run.ID, run.Status, deref(run.Error))
	}

	prBody := livePRBody(t, statePath, branch)
	if !strings.Contains(prBody, "settled by the operator without a fix") {
		t.Fatalf("the PR body does not report the settled finding as settled without a fix:\n%s", prBody)
	}
	reviewBlock := publishedStepDetails(prBody, "Review")
	if reviewBlock == "" {
		t.Fatalf("the PR body has no Review section:\n%s", prBody)
	}
	if strings.Contains(reviewBlock, "✅ No issues found.") || strings.Contains(reviewBlock, "✅ Re-checked - no issues remain.") {
		t.Fatalf("a Review round that only stopped reporting a settled finding is published as a clean round:\n%s", reviewBlock)
	}
	if !strings.Contains(reviewBlock, "settled by the operator without a fix") {
		t.Fatalf("the Review section does not report the settlement:\n%s", reviewBlock)
	}
	if strings.Contains(prBody, privateReason) || strings.Contains(prBody, "private infra repository") {
		t.Fatalf("the operator's private settlement reason was published to the PR:\n%s", prBody)
	}
}

// publishedStepDetails returns the published PR body's collapsible section for
// one step, so an assertion about a Review round cannot be satisfied (or
// defeated) by another step's line.
func publishedStepDetails(body, step string) string {
	marker := "**" + step + "**"
	start := strings.Index(body, marker)
	if start < 0 {
		return ""
	}
	block := body[start:]
	if end := strings.Index(block, "</details>"); end > 0 {
		block = block[:end]
	}
	return block
}
