//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewAddedTwinPreservesTheIgnoredGateInstance(t *testing.T) {
	const concern = "missing check"
	scenario := filepath.Join(t.TempDir(), "agent.yaml")
	body := `actions:
  - match: "Fix-round provenance:"
    structured:
      findings:
        - id: A
          severity: warning
          file: deploy.sh
          line: 10
          description: missing check
          action: ask-user
          review_scope: source
        - id: B
          severity: warning
          file: deploy.sh
          line: 20
          description: missing check
          action: ask-user
          review_scope: source
        - id: C
          severity: warning
          file: deploy.sh
          line: 30
          description: missing cleanup
          action: ask-user
          review_scope: source
      summary: concerns remain
      risk_level: medium
      risk_rationale: checks remain missing
      risk_scope: source-or-external
      reviewed_paths: [deploy.sh]
  - match: "Investigate previous review findings"
    structured:
      findings: []
      summary: attempted the selected fix
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: A
          severity: warning
          file: deploy.sh
          line: 10
          description: missing check
          action: ask-user
          review_scope: source
      summary: missing check
      risk_level: medium
      risk_rationale: check is missing
      risk_scope: source-or-external
      reviewed_paths: [deploy.sh]
  - structured:
      findings: []
      summary: clean
      risk_level: low
      risk_rationale: nothing else to report
      risk_scope: source-or-external
`
	if err := os.WriteFile(scenario, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange("feature/added-twin", "deploy.sh", "#!/usr/bin/env bash\ndeploy\n", "add deploy script")
	if out, err := h.Run("axi", "run", "--intent", "Check the deploy script", "--skip", "ci"); err != nil || !strings.Contains(out, concern) {
		t.Fatalf("initial review gate: %v\n%s", err, out)
	}
	addition := `{"id":"B","file":"deploy.sh","line":20,"severity":"warning","description":"missing check"}`
	if out, err := h.Run("axi", "respond", "--action", "fix", "--ignore", "A", "--add-finding", addition); err != nil {
		t.Fatalf("fix the addition and ignore the gate instance: %v\n%s", err, out)
	}

	type roundView struct {
		SelectionSource    string   `json:"selection_source"`
		SelectedFindingIDs []string `json:"selected_finding_ids"`
		Findings           []struct {
			ID string `json:"id"`
		} `json:"findings"`
	}
	readRounds := func() []roundView {
		t.Helper()
		out, err := h.Run("axi", "rounds", "--step", "review", "--json")
		if err != nil {
			t.Fatalf("export rounds: %v\n%s", err, out)
		}
		var export struct {
			Rounds []roundView `json:"rounds"`
		}
		if err := json.Unmarshal([]byte(out), &export); err != nil {
			t.Fatalf("decode rounds: %v\n%s", err, out)
		}
		return export.Rounds
	}
	rounds := readRounds()
	if len(rounds) != 2 || rounds[0].SelectionSource != "user" || len(rounds[0].SelectedFindingIDs) != 1 || rounds[0].SelectedFindingIDs[0] != "B" {
		t.Fatalf("addition was not the sole selected fix: %+v", rounds)
	}
	if len(rounds[1].Findings) != 2 {
		t.Fatalf("accepted ignore did not suppress A: %+v", rounds[1])
	}
	for _, item := range rounds[1].Findings {
		if item.ID != "B" && item.ID != "C" {
			t.Fatalf("ignored gate instance returned: %+v", rounds[1])
		}
	}
	for _, args := range [][]string{
		{"axi", "respond", "--step", "review", "--action", "fix", "--ignore", "B", "--findings", "C"},
		{"axi", "respond", "--step", "review", "--action", "resolved_external", "--findings", "B", "--reason", "handled elsewhere"},
	} {
		out, err := h.Run(args...)
		if err == nil || !strings.Contains(out, "already chosen to fix in an earlier round") {
			t.Fatalf("selected addition lost exact protection: %v\n%s", err, out)
		}
	}
	if out, err := h.Run("axi", "respond", "--step", "review", "--action", "fix", "--findings", "C"); err != nil {
		t.Fatalf("omit the earlier selected addition: %v\n%s", err, out)
	}
	rounds = readRounds()
	if len(rounds) != 3 || len(rounds[2].Findings) != 2 {
		t.Fatalf("ignore did not survive another round: %+v", rounds)
	}
	foundIgnore := false
	for _, inv := range h.AgentInvocations() {
		if !strings.Contains(inv.Prompt, "Fix-round provenance:") {
			continue
		}
		inIgnore := false
		for _, line := range strings.Split(inv.Prompt, "\n") {
			if line == "user_chose_to_ignore:" {
				inIgnore = true
				continue
			}
			if !strings.HasPrefix(line, "  - ") {
				inIgnore = false
				continue
			}
			if !inIgnore {
				continue
			}
			var item struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "  - ")), &item); err != nil {
				t.Fatalf("decode emitted decision: %v", err)
			}
			if item.ID == "B" {
				t.Fatal("emitted history labels the selected addition as ignored")
			}
			foundIgnore = foundIgnore || item.ID == "A"
		}
	}
	if !foundIgnore {
		t.Fatal("emitted review history lost the accepted ignore of A")
	}
	out, _ := h.Run("axi", "respond", "--step", "review", "--action", "abort", "--reason", "finished testing added twins")
	if !strings.Contains(out, "rejected:") {
		t.Fatalf("gate did not remain answerable: %s", out)
	}
}
