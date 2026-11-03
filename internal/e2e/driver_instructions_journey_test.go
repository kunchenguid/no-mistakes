//go:build e2e

package e2e

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// driverGuidance is the scoping an agent driver passes with
// `axi respond --instructions`. It is distinctive so every prompt can be
// searched for it.
const driverGuidance = "DRIVER-SCOPE only touch feature.txt and leave the docs alone"

// driverInstructionsScenario reports one ask-user finding, lets the fixer edit
// the file, and has the rereview report a second ask-user finding so the run
// parks again after the fix round. The rereview carries the fix-round
// provenance clause, so it is listed first.
func driverInstructionsScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "driver-instructions-scenario.yaml")
	content := `actions:
  - match: "Fix-round provenance:"
    structured:
      findings:
        - id: r2
          severity: warning
          file: feature.txt
          line: 1
          description: the default is still undocumented
          action: ask-user
      summary: one follow-on finding
      risk_level: medium
      risk_rationale: a default is undocumented
      risk_scope: source-or-external
  - match: "Investigate previous review findings"
    edits:
      - path: feature.txt
        new: "flag = false\n"
    structured:
      findings: []
      summary: flipped the default
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: r1
          severity: warning
          file: feature.txt
          line: 1
          description: the flag ships enabled for existing installations
          action: ask-user
      summary: one finding
      risk_level: medium
      risk_rationale: a default changes for existing installations
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
      title: flip the flag default
      body: Flip the flag default
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func driverJourneyStart(t *testing.T, h *Harness, branch string) {
	t.Helper()
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange(branch, "feature.txt", "flag = true\n", "add the feature flag")
	gate, err := h.Run("axi", "run", "--intent", "Add the feature flag")
	if err != nil || !strings.Contains(gate, "r1") {
		t.Fatalf("initial review gate: %v\n%s", err, gate)
	}
	next, err := h.Run("axi", "respond", "--action", "fix", "--findings", "r1", "--instructions", driverGuidance)
	if err != nil || !strings.Contains(next, "r2") {
		t.Fatalf("fix response with driver instructions: %v\n%s", err, next)
	}
	t.Logf("axi respond --action fix --instructions output:\n%s", next)
}

func promptsWith(h *Harness, marker string) []string {
	var out []string
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, marker) {
			out = append(out, inv.Prompt)
		}
	}
	return out
}

// The driver's --instructions reach the fixer and the step's own rereview as
// driver guidance, are persisted as driver_instructions, and never appear as
// the user's decision in the decision history the Test step receives.
func TestDriverInstructionsJourney_RecordedAsDriverGuidance(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: driverInstructionsScenario(t)})
	branch := "feature/driver-instructions"
	driverJourneyStart(t, h, branch)

	fixPrompts := promptsWith(h, "Investigate previous review findings")
	if len(fixPrompts) == 0 || !strings.Contains(fixPrompts[0], driverGuidance) {
		t.Fatalf("the fixer never received the driver's guidance")
	}

	rereviews := promptsWith(h, "Fix-round provenance:")
	if len(rereviews) == 0 {
		t.Fatal("no rereview turn ran")
	}
	rereview := rereviews[len(rereviews)-1]
	if !strings.Contains(rereview, `"driver_instructions":"`+driverGuidance+`"`) {
		t.Errorf("the rereview's own round history does not carry the guidance as driver_instructions")
	}
	if !strings.Contains(rereview, "driver_instructions field is fix guidance from the agent driving this run, not a decision by the user") {
		t.Errorf("the rereview's own round history does not say whose guidance it is")
	}
	if at := strings.Index(rereview, "Round 1 ("); at >= 0 {
		block := rereview[at:]
		if len(block) > 1500 {
			block = block[:1500]
		}
		t.Logf("rereview own-step round history:\n%s", block)
	}

	// Persisted state: the round's selection holds the note as driver
	// guidance, never as user_instructions.
	database, err := db.OpenReadOnly(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	run := h.ActiveRun(branch)
	if run == nil {
		t.Fatal("no active run")
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var recorded string
	for _, s := range steps {
		if s.StepName != types.StepReview {
			continue
		}
		rounds, err := database.GetRoundsByStep(s.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rounds {
			if r.UserFindingsJSON != nil && strings.Contains(*r.UserFindingsJSON, "r1") {
				recorded = *r.UserFindingsJSON
				t.Logf("review round %d selection_source=%v user_findings_json=%s", r.Round, deref(r.SelectionSource), recorded)
			}
		}
	}
	database.Close()
	if !strings.Contains(recorded, `"driver_instructions":"`+driverGuidance+`"`) || strings.Contains(recorded, `"user_instructions"`) {
		t.Fatalf("the recorded selection does not hold the guidance as driver_instructions only: %s", recorded)
	}

	// Approve the follow-on finding; the Test step's prompt carries the
	// run's decision history.
	out, err := h.Run("axi", "respond", "--action", "approve")
	if err != nil {
		t.Fatalf("approve review: %v\n%s", err, out)
	}
	const decisions = "Decisions already made by the user in this run"
	var section string
	for _, p := range promptsWith(h, decisions) {
		if !strings.Contains(p, "review round 1 user chose to fix") {
			continue
		}
		section = p[strings.Index(p, decisions):]
		if strings.Contains(section, driverGuidance) {
			t.Fatalf("a later step's decision history presents the driver's guidance as the user's decision:\n%s", section)
		}
	}
	if section == "" {
		t.Fatal("no later step received the review fix decision in its decision history")
	}
	if len(section) > 1500 {
		section = section[:1500]
	}
	t.Logf("later step's decision history:\n%s", section)
}

// A superseding run's reviewer receives the previous run's rounds and branch
// decisions without the driver's guidance.
func TestDriverInstructionsJourney_SupersedingRunOmitsDriverGuidance(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: driverInstructionsScenario(t)})
	pushMainRepoConfig(t, h, trustedRepoConfigWithReviewConversation)
	branch := "feature/driver-instructions-supersede"
	driverJourneyStart(t, h, branch)
	before := len(h.AgentInvocations())

	h.CommitChange(branch, "feature.txt", "flag = false\nmore = 1\n", "author pushes again")
	h.PushToGate(branch)

	var prompt string
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) && prompt == "" {
		invs := h.AgentInvocations()
		for _, inv := range invs[min(before, len(invs)):] {
			if strings.Contains(inv.Prompt, "Previous run's review rounds on this branch:") ||
				strings.Contains(inv.Prompt, "Decisions already made by the user on this branch in earlier runs") {
				prompt = inv.Prompt
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if prompt == "" {
		t.Fatal("the superseding run's reviewer never received the previous run's history")
	}
	for _, heading := range []string{"Previous run's review rounds on this branch:", "Decisions already made by the user on this branch in earlier runs"} {
		if at := strings.Index(prompt, heading); at >= 0 {
			section := prompt[at:]
			if len(section) > 1500 {
				section = section[:1500]
			}
			t.Logf("superseding run section %q:\n%s", heading, section)
		}
	}
	if !strings.Contains(prompt, "user_chose_to_fix") && !strings.Contains(prompt, "user chose to fix") {
		t.Errorf("the superseding run's history lost the user's fix decision")
	}
	if strings.Contains(prompt, driverGuidance) {
		at := strings.Index(prompt, driverGuidance)
		t.Fatalf("the superseding run's reviewer was shown the driver's guidance:\n...%s...", prompt[max(0, at-600):at+len(driverGuidance)])
	}
}

// An added finding cannot carry driver guidance: `--add-finding` JSON with a
// driver_instructions key is refused before anything reaches the daemon, so
// the gate stays parked on its own findings, and the same addition without
// that key is accepted.
func TestDriverInstructionsJourney_AddedFindingRefusesDriverInstructions(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: driverInstructionsScenario(t)})
	branch := "feature/driver-instructions-add-finding"
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange(branch, "feature.txt", "flag = true\n", "add the feature flag")
	gate, err := h.Run("axi", "run", "--intent", "Add the feature flag")
	if err != nil || !strings.Contains(gate, "r1") {
		t.Fatalf("initial review gate: %v\n%s", err, gate)
	}
	before := len(h.AgentInvocations())

	refused, err := h.Run("axi", "respond", "--action", "fix", "--findings", "r1",
		"--add-finding", `{"description":"also document the default","driver_instructions":"`+driverGuidance+`"}`)
	t.Logf("axi respond --add-finding with driver_instructions (err=%v):\n%s", err, refused)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("expected exit 2, got %v", err)
	}
	if !strings.Contains(refused, "invalid --add-finding") || !strings.Contains(refused, "driver_instructions") {
		t.Fatalf("refusal does not name the rejected key:\n%s", refused)
	}
	if n := len(h.AgentInvocations()); n != before {
		t.Fatalf("the refused response still started %d agent turn(s)", n-before)
	}
	status, err := h.Run("axi", "status")
	t.Logf("axi status after the refusal:\n%s", status)
	if err != nil || !strings.Contains(status, "r1") {
		t.Fatalf("gate no longer parked on r1 after the refusal: %v\n%s", err, status)
	}

	accepted, err := h.Run("axi", "respond", "--action", "fix", "--findings", "r1",
		"--add-finding", `{"description":"also document the default","user_instructions":"mention it in the README"}`)
	t.Logf("axi respond --add-finding without driver_instructions (err=%v):\n%s", err, accepted)
	if err != nil || !strings.Contains(accepted, "r2") {
		t.Fatalf("the same addition without driver_instructions was not accepted: %v\n%s", err, accepted)
	}
	for _, p := range promptsWith(h, "Investigate previous review findings") {
		if strings.Contains(p, driverGuidance) {
			t.Fatal("the refused driver guidance reached the fixer")
		}
	}
}
