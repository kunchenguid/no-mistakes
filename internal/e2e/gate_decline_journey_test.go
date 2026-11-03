//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A human driving a review gate asks for every finding in round 1, then asks
// for only the NEW findings in round 2 - the partial selection a real run made,
// where the driving agent listed the eight ids it meant to fix and silently
// dropped the five the reviewer carried forward. The record must keep the
// round-1 fix decisions instead of reading that omission as a decline, which is
// what told a later review round to undo work the same person had asked for.
//
// The gate shape is produced the way a real run produces it: the reviewer's
// rereview reports two more findings and claims no coverage of the earlier
// files, so the five round-1 findings stay outstanding and reappear in the
// round-2 gate next to the new ones.
func TestGateDeclineJourney_PartialSelectionKeepsEarlierFixes(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "agent.yaml")
	// Actions are matched in order against the prompt, so the rereview (which
	// carries the fix-round provenance clause) must precede the plain review
	// action, and the fix turn matches its own prompt.
	body := `actions:
  - match: "Fix-round provenance:"
    structured:
      findings:
        - id: r6
          severity: error
          file: snapshot.sh
          line: 12
          description: the snapshot tarball is written into the repository root instead of the artifacts directory
          action: ask-user
          review_scope: source
        - id: r7
          severity: warning
          file: deploy.sh
          line: 48
          description: cleanup never runs when the deploy fails, leaving the staging directory behind
          action: auto-fix
          review_scope: source
      summary: two follow-on findings
      risk_level: medium
      risk_rationale: the rereview found two more issues in the deploy path
      risk_scope: source-or-external
  - match: "Investigate previous review findings"
    edits:
      - path: deploy.sh
        new: |
          #!/usr/bin/env bash
          set -euo pipefail
          deploy_env="${DEPLOY_ENV:?set DEPLOY_ENV}"
          health() { curl -fsS "https://${deploy_env}.example.com/healthz" >/dev/null; }
          deploy() { tar -xzf build.tar.gz -C /srv/app; }
          deploy
          health
          echo "deployed to ${deploy_env}"
    structured:
      findings: []
      summary: applied the selected fixes
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: r1
          severity: error
          file: deploy.sh
          line: 4
          description: the allow-dirty bypass lets a dirty working tree ship uncommitted files
          action: ask-user
          review_scope: source
        - id: r2
          severity: warning
          file: deploy.sh
          line: 2
          description: set -u is missing so an unset DEPLOY_ENV expands to an empty value
          action: ask-user
          review_scope: source
        - id: r3
          severity: warning
          file: deploy.sh
          line: 15
          description: the compose heredoc expands variables because its delimiter is unquoted
          action: auto-fix
          review_scope: source
        - id: r4
          severity: error
          file: snapshot.sh
          line: 6
          description: committed snapshot tarballs land in the repository and every deploy adds a binary blob
          action: ask-user
          review_scope: source
        - id: r5
          severity: warning
          file: deploy.sh
          line: 31
          description: the health check exit status is ignored so a failed deploy reports success
          action: auto-fix
          review_scope: source
      summary: five findings
      risk_level: high
      risk_rationale: the deploy path can ship uncommitted files and report success after a failed health check
      risk_scope: source-or-external
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
      title: fix deploy hardening
      body: Deploy hardening
`
	if err := os.WriteFile(scenario, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	branch := "feature/deploy-hardening"
	h.CommitChange(branch, "deploy.sh", `#!/usr/bin/env bash
# deploy the staged build
./build.sh --allow-dirty
tar -czf snapshot.tar.gz /srv/app
cat <<EOF > compose.yaml
image: app:${TAG}
EOF
curl "https://example.com/healthz" || true
echo deployed
`, "add deploy script")
	h.CommitChange(branch, "snapshot.sh", `#!/usr/bin/env bash
# snapshot the release artifacts
tar -czf snapshot.tar.gz /srv/app
git add snapshot.tar.gz
`, "add snapshot script")

	initial, err := h.Run("axi", "run", "--intent", "Harden the deploy script: ship only committed files, fail loudly on a bad environment, and keep release artifacts out of the repository")
	if err != nil || !strings.Contains(initial, "r1") || !strings.Contains(initial, "r5") {
		t.Fatalf("initial review gate: %v\n%s", err, initial)
	}

	// Round 1: the human asks for every finding to be fixed.
	round2, err := h.Run("axi", "respond", "--action", "fix", "--findings", "r1,r2,r3,r4,r5")
	if err != nil {
		t.Fatalf("round 1 fix response: %v\n%s", err, round2)
	}
	// The round-2 gate shows the carried findings next to the new ones, which
	// is the shape that makes the partial response dangerous.
	for _, want := range []string{"r1", "r5", "r6", "r7"} {
		if !strings.Contains(round2, want) {
			t.Fatalf("round 2 gate does not carry %s:\n%s", want, round2)
		}
	}

	// Round 2: the human asks for only the new findings, exactly as the real
	// run's driver did.
	round3, err := h.Run("axi", "respond", "--action", "fix", "--findings", "r6,r7")
	if err != nil {
		t.Fatalf("round 2 partial fix response: %v\n%s", err, round3)
	}

	// The next review turn is told what the human actually decided: the
	// round-1 fixes are still fixes, and the round-2 omission did not decline
	// them. Only round 2's own block is checked, and only a block that
	// actually lists declined findings counts as a decline.
	reviewed := false
	for _, inv := range h.AgentInvocations() {
		round2At := strings.Index(inv.Prompt, "Round 2 (auto_fix)")
		if round2At < 0 {
			continue
		}
		reviewed = true
		block := inv.Prompt[round2At:]
		if next := strings.Index(block[1:], "Round 3 ("); next > 0 {
			block = block[:next+1]
		}
		ignoreAt := strings.Index(block, "user_chose_to_ignore:")
		if ignoreAt < 0 {
			continue
		}
		var declined []string
		for _, line := range strings.Split(block[ignoreAt:], "\n")[1:] {
			if !strings.HasPrefix(line, "  - ") {
				break
			}
			declined = append(declined, strings.TrimSpace(line))
		}
		if len(declined) > 0 {
			t.Fatalf("round 2's partial selection re-declined the findings fixed in round 1; the next review turn is told:\nRound 2 (auto_fix) user_chose_to_ignore:\n  %s", strings.Join(declined, "\n  "))
		}
	}
	if !reviewed {
		t.Fatalf("no review turn saw the round history; agent invocations: %d", len(h.AgentInvocations()))
	}

	// The echo reports the dispositions the response recorded: r6 and r7 fixed,
	// the carried r1-r5 kept rather than declined.
	// The recorded: block is the evidence: the gate table above it also names r1,
	// so a bare substring search would pass without the disposition being
	// reported at all.
	echo := recordedDispositionsBlock(round3)
	if echo == "" {
		t.Fatalf("round 2 response has no recorded: block:\n%s", round3)
	}
	kept := ""
	for _, line := range strings.Split(echo, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "kept") {
			kept = line
			break
		}
	}
	for _, id := range []string{"r1", "r5"} {
		if !strings.Contains(kept, id) {
			t.Fatalf("recorded kept line %q does not report the carried %s:\n%s", kept, id, round3)
		}
	}

	t.Logf("journey: %d agent turns; the round-2 gate carried r1-r5 and the partial response kept them", len(h.AgentInvocations()))
}

// recordedDispositionsBlock returns the echo's recorded: section - the part of
// the output that reports what the response actually recorded - so an assertion
// cannot be satisfied by the gate table printed above it.
func recordedDispositionsBlock(out string) string {
	lines := strings.Split(out, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "recorded:") {
			start = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	var b strings.Builder
	for _, line := range lines[start:] {
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "recorded:") {
			break
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}
