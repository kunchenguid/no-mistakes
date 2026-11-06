//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The other half of the settlement contract: an explicit --ignore on a Review
// gate stays effective after the reviewer re-mints the finding's id and moves
// its line, while a finding the same operator chose to FIX keeps being reported
// and can be settled by neither disposition.
//
// The oracle is the Coverage continuation section of
// docs/src/content/docs/reference/pipeline-steps.md ("An explicit --ignore in a
// Review fix response remains effective across later coverage rounds, even when
// the reviewer changes the finding ID or line" and "A finding an earlier round
// of the same Review chose to fix is refused by both dispositions, however a
// later review turn re-mints its ID or moves its line").
func TestReviewStickyIgnoreSurvivesARemintedFinding(t *testing.T) {
	const tokenConcern = "the deploy ships the staged build without checking the health endpoint exit status"
	const tarballConcern = "every release writes its tarball into the repository root instead of the artifacts directory"

	scenario := filepath.Join(t.TempDir(), "agent.yaml")
	body := `actions:
  - match: "Fix-round provenance:"
    structured:
      findings:
        - id: health-moved
          severity: error
          file: deploy.sh
          line: 21
          description: ` + tokenConcern + `
          action: ask-user
          review_scope: source
        - id: tarball-moved
          severity: warning
          file: snapshot.sh
          line: 19
          description: ` + tarballConcern + `
          action: ask-user
          review_scope: source
      summary: both concerns are still visible
      risk_level: high
      risk_rationale: the deploy path still reports success after a failed health check
      risk_scope: source-or-external
      reviewed_paths: [deploy.sh]
  - match: "Investigate previous review findings"
    edits:
      - path: deploy.sh
        new: |
          #!/usr/bin/env bash
          set -euo pipefail
          deploy --token "${DEPLOY_TOKEN}"
    structured:
      findings: []
      summary: applied the selected fix
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: health
          severity: error
          file: deploy.sh
          line: 4
          description: ` + tokenConcern + `
          action: ask-user
          review_scope: source
        - id: tarball
          severity: warning
          file: snapshot.sh
          line: 6
          description: ` + tarballConcern + `
          action: ask-user
          review_scope: source
      summary: two concerns
      risk_level: high
      risk_rationale: the deploy path can report success after a failed health check
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
      title: "fix: deploy hardening"
      body: Deploy hardening
`
	if err := os.WriteFile(scenario, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/sticky-ignore"
	h.CommitChange(branch, "deploy.sh", "#!/usr/bin/env bash\n# deploy the staged build\ndeploy --token \"${DEPLOY_TOKEN}\"\ncurl https://example.com/healthz || true\n", "add deploy script")
	h.CommitChange(branch, "snapshot.sh", "#!/usr/bin/env bash\n# archive the release\ntar -czf app.tar.gz /srv/app\n", "add snapshot script")

	gate1, err := h.Run("axi", "run", "--intent", "Harden the deploy script and keep release archives out of the repository", "--skip", "ci")
	if err != nil || !strings.Contains(gate1, "health") || !strings.Contains(gate1, "tarball") {
		t.Fatalf("initial review gate: %v\n%s", err, gate1)
	}
	settlementEvidence(t, "10-sticky-gate-round1.txt", "$ no-mistakes axi run --intent ... --skip ci\n"+gate1)

	gate2, err := h.Run("axi", "respond", "--action", "fix", "--findings", "health", "--ignore", "tarball")
	if err != nil {
		t.Fatalf("fix one finding and decline the other: %v\n%s", err, gate2)
	}
	settlementEvidence(t, "11-sticky-gate-round2.txt", "$ no-mistakes axi respond --action fix --findings health --ignore tarball\n"+gate2)

	// The reviewer re-reported BOTH concerns under new ids at new lines. The
	// declined one must stay off the gate; the one the operator chose to fix
	// must still be reported.
	if strings.Contains(gate2, "tarball-moved") || strings.Contains(gate2, "artifacts directory") {
		t.Fatalf("the declined finding came back under a new id and line:\n%s", gate2)
	}
	if !strings.Contains(gate2, tokenConcern) {
		t.Fatalf("the finding the operator chose to fix is no longer reported:\n%s", gate2)
	}

	// The round export is the documented machine-readable record of what the
	// two rounds held: round 1's user selection, and round 2 still carrying the
	// concern the operator chose to fix (under the identity the outstanding set
	// retains for it) while the declined concern is gone from the record too,
	// not merely hidden in the gate table.
	rounds, err := h.Run("axi", "rounds", "--step", "review", "--json")
	if err != nil {
		t.Fatalf("axi rounds: %v\n%s", err, rounds)
	}
	settlementEvidence(t, "11b-sticky-rounds.json", rounds)
	var export struct {
		Rounds []struct {
			Round           int    `json:"round"`
			Trigger         string `json:"trigger"`
			SelectionSource string `json:"selection_source"`
			Findings        []struct {
				ID          string `json:"id"`
				Description string `json:"description"`
			} `json:"findings"`
		} `json:"rounds"`
	}
	if err := json.Unmarshal([]byte(rounds), &export); err != nil {
		t.Fatalf("parse axi rounds export: %v\n%s", err, rounds)
	}
	if len(export.Rounds) < 2 {
		t.Fatalf("review recorded %d round(s), want at least 2:\n%s", len(export.Rounds), rounds)
	}
	if got := export.Rounds[0].SelectionSource; got != "user" {
		t.Fatalf("round 1 selection_source = %q, want user:\n%s", got, rounds)
	}
	carried := false
	for _, item := range export.Rounds[1].Findings {
		if item.Description == tarballConcern {
			t.Fatalf("round 2 still records the declined concern:\n%s", rounds)
		}
		if item.Description == tokenConcern {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("round 2 does not record the concern the operator chose to fix:\n%s", rounds)
	}

	// Adversarial: neither disposition may retract that applied fix, however
	// the reviewer re-minted the id or moved the line.
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"settle an earlier fix", []string{"axi", "respond", "--step", "review", "--action", "resolved_external", "--findings", "health", "--reason", "handled elsewhere"}},
		{"decline an earlier fix", []string{"axi", "respond", "--step", "review", "--action", "fix", "--ignore", "health"}},
	} {
		out, err := h.Run(tc.args...)
		settlementEvidence(t, "12-refused-"+strings.ReplaceAll(tc.name, " ", "-")+".txt", "$ no-mistakes "+strings.Join(tc.args, " ")+"\n"+out)
		if err == nil {
			t.Fatalf("%s: the response was accepted:\n%s", tc.name, out)
		}
		if !strings.Contains(out, "already chosen to fix in an earlier round") {
			t.Fatalf("%s: refusal does not name the earlier fix:\n%s", tc.name, out)
		}
	}

	// The gate survived both refusals and still accepts an ordinary verdict.
	// An abort ends the run failed, so the command's own exit status is
	// nonzero by design; the recorded rejection is what proves the gate was
	// still answerable after both refusals.
	out, _ := h.Run("axi", "respond", "--step", "review", "--action", "abort", "--reason", "ending the journey after the dispositions were exercised")
	if !strings.Contains(out, "rejected:") || !strings.Contains(out, "ending the journey after the dispositions were exercised") {
		t.Fatalf("abort did not record the operator's rejection:\n%s", out)
	}
	settlementEvidence(t, "13-sticky-abort.txt", "$ no-mistakes axi respond --step review --action abort --reason \"...\"\n"+out)
}
