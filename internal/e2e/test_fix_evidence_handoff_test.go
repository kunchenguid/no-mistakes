//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #1774: the evidence turn that follows a Test fix turn in the same
// execution must see that fix turn's own structured summary, its outcome
// (including "no changes applied"), and the evidence files it wrote, framed as
// unverified metadata. This drives the real CLI, daemon, and Test step with a
// scripted agent: the first evidence turn reports no-go, the operator asks for
// a fix, the fix turn writes one evidence file (and changes no code), and the
// prompt the next evidence turn receives is inspected.
func TestTestFixTurnHandsItsSummaryAndEvidenceToTheEvidenceTurn(t *testing.T) {
	const (
		branch      = "feature/fix-evidence-handoff"
		fixSentinel = "FIX-SENTINEL reproduced the failing scenario and wrote fix-turn/recording.txt"
		evStart     = "You are validating a code change by driving the product itself."
		fixStart    = "Fix the failing tests in this repository."
	)
	scenario := filepath.Join(t.TempDir(), "scenario.yaml")
	writeScenario := func(fixEvidencePath string) {
		t.Helper()
		fixGit := ""
		if fixEvidencePath != "" {
			// The fix prompt does not name the evidence directory, so the
			// scripted fixer writes its file by absolute path via git config.
			fixGit = `
    git:
      - ["config", "--file", "` + fixEvidencePath + `", "fix.note", "fix-turn-recording"]`
		}
		data := `actions:
  - match: "Current execution fix turn"
    text: "evidence after fix"
    structured:
      findings: []
      summary: "re-driven"
      tested: ["fakeagent: re-drove scenario"]
      testing_summary: "re-driven after fix"
      artifacts: []
      verdict: go
      scenarios:
        - name: synthetic scenario
          result: pass
          live: true
          evidence: "fakeagent: re-driven"
          reason: ""
  - match: "` + fixStart + `"
    text: "fixed"
    structured:
      summary: "` + fixSentinel + `"` + fixGit + `
  - match: "` + evStart + `"
    text: "first evidence turn"
    write_evidence:
      - path: earlier-round.txt
        content: "earlier round evidence"
    structured:
      findings: []
      summary: "failed"
      tested: ["fakeagent: drove scenario"]
      testing_summary: "scenario failed"
      artifacts: []
      verdict: no-go
      scenarios:
        - name: synthetic scenario
          result: fail
          live: true
          evidence: "fakeagent: simulated failure"
          reason: ""
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
	writeScenario("")

	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange(branch, ".no-mistakes.yaml", "auto_fix:\n  test: 0\ncommands:\n  lint: 'exit 0'\n", "configure")
	h.CommitChange(branch, "feature.txt", "synthetic feature\n", "add feature")
	out, err := h.Run("axi", "run", "--intent", "Validate the synthetic feature", "--skip", "pr,ci")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	gated := waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusAwaitingApproval, 60*time.Second)

	// Scenario: an initial (non-fix) evidence turn carries no fix-turn section.
	var firstEvidence string
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, evStart) {
			firstEvidence = inv.Prompt
		}
	}
	if firstEvidence == "" {
		t.Fatal("no initial evidence turn recorded")
	}
	if strings.Contains(firstEvidence, "Current execution fix turn") || strings.Contains(firstEvidence, "fix_summary:") {
		t.Fatalf("initial evidence turn gained a fix-turn section:\n%s", firstEvidence)
	}
	t.Logf("SCENARIO non-fix evidence turn: no fix-turn section present (prompt %d bytes)", len(firstEvidence))

	evidenceDir := filepath.Join(h.NMHome, "evidence", gated.ID)
	if _, err := os.Stat(filepath.Join(evidenceDir, "earlier-round.txt")); err != nil {
		t.Fatalf("earlier-round evidence not written: %v", err)
	}
	fixFile := filepath.Join(evidenceDir, "fix-turn", "recording.txt")
	if err := os.MkdirAll(filepath.Dir(fixFile), 0o755); err != nil {
		t.Fatal(err)
	}
	writeScenario(fixFile)

	database, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	steps, err := database.GetStepsByRun(gated.ID)
	database.Close()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, st := range steps {
		if st.StepName == types.StepTest && st.FindingsJSON != nil {
			fs, perr := types.ParseFindingsJSON(*st.FindingsJSON)
			if perr != nil {
				t.Fatal(perr)
			}
			for _, f := range fs.Items {
				ids = append(ids, f.ID)
			}
		}
	}
	if len(ids) == 0 {
		t.Fatal("no Test findings at the no-go gate")
	}
	out, err = h.Run("axi", "respond", "--step", "test", "--action", "fix", "--findings", strings.Join(ids, ","))
	if err != nil {
		t.Fatalf("respond fix: %v\n%s", err, out)
	}
	t.Logf("axi respond --action fix output:\n%s", out)

	var after string
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, "Current execution fix turn") {
			after = inv.Prompt
		}
	}
	if after == "" {
		t.Fatalf("no evidence turn received the current fix-turn section; invocations=%d", len(h.AgentInvocations()))
	}
	i := strings.Index(after, "Current execution fix turn")
	section := after[i:]
	if j := strings.Index(section, "\n\n"); j > 0 {
		section = section[:j]
	}
	t.Logf("SECTION delivered to evidence turn:\n%s", section)

	// Scenario: summary, outcome "no changes applied", and the new evidence file.
	for _, want := range []string{
		"fix_summary: \"" + fixSentinel + "\"",
		"no changes applied",
		fixFile,
		"metadata only, not verified evidence",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("fix-turn section missing %q:\n%s", want, section)
		}
	}
	// Adversarial: the earlier round's file is not attributed to this fix turn.
	if strings.Contains(section, "earlier-round.txt") {
		t.Fatalf("earlier-round evidence listed as fix-turn evidence:\n%s", section)
	}
	// The evidence-turn obligations remain.
	if !strings.Contains(after, evStart) || !strings.Contains(after, "Report every scenario in the \"scenarios\" array") {
		t.Fatalf("evidence turn lost its own obligations:\n%s", after)
	}

	final := h.WaitForRun(branch, 60*time.Second)
	t.Logf("final run status: %s", final.Status)

	// The stored round summary contract is unchanged: the fix round persists the
	// fix outcome, not the evidence-turn metadata section.
	rdb, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	finalSteps, err := rdb.GetStepsByRun(final.ID)
	if err != nil {
		t.Fatal(err)
	}
	sawFixRound := false
	for _, st := range finalSteps {
		if st.StepName != types.StepTest {
			continue
		}
		rounds, err := rdb.GetRoundsByStep(st.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rounds {
			fs := "<nil>"
			if r.FixSummary != nil {
				fs = *r.FixSummary
			}
			t.Logf("STORED round %d trigger=%s fix_summary=%q", r.Round, r.Trigger, fs)
			if r.Trigger == "auto_fix" || r.Trigger == "user_fix" || r.FixSummary != nil {
				sawFixRound = true
				if r.FixSummary == nil || strings.Contains(*r.FixSummary, "Current execution fix turn") {
					t.Fatalf("fix round stored summary changed: %q", fs)
				}
			}
		}
	}
	if !sawFixRound {
		t.Fatal("no stored Test fix round")
	}
	if final.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed", final.Status)
	}
}
