//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// carryoverIntent is the operator's stated intent for the carry-forward
// journey, distinctive enough to tell it apart in a log.
const carryoverIntent = "harden the feature file without dropping either reported concern"

// carryoverScenario drives the exact shape the carry-forward fix exists for:
//
//   - The initial review reports TWO ask-user findings (review auto-fix is off
//     by default, so both park for the operator).
//   - The fixer round matches the fix prompt and edits the worktree.
//   - The re-review after a fix round is scoped to the fix diff and legitimately
//     reports NOTHING new. It is matched on "Fix-round provenance:", a clause the
//     review prompt only carries on a post-fix re-review, so the initial review
//     and the re-review get different canned answers even though they share the
//     rest of the prompt.
//
// The fixer's edit rewrites feature.txt wholesale (empty `old`), which makes it
// idempotent: the first fix round produces a real fix commit, a later one is a
// no-op the pipeline records as "no agent changes to commit".
func carryoverScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "carryover-scenario.yaml")
	content := `actions:
  - match: "Fix-round provenance:"
    text: "re-review of the fix diff found nothing new"
    structured:
      findings: []
      summary: "the fix diff is clean"
      risk_level: low
      risk_rationale: "the fix round introduced no new risk"
  - match: "Investigate previous review findings"
    text: "guarded the unsafe value"
    edits:
      - path: "feature.txt"
        new: "safe\nhardcoded-timeout\n"
    structured:
      summary: "guard the unsafe value"
  - match: "Review the code changes and return structured findings"
    text: "review found two issues needing a decision"
    structured:
      findings:
        - id: "carry-1"
          severity: error
          file: "feature.txt"
          line: 1
          description: "unsafe value reaches the loader without validation"
          action: ask-user
        - id: "carry-2"
          severity: warning
          file: "feature.txt"
          line: 2
          description: "hardcoded timeout should be configurable"
          action: ask-user
      summary: "found 2 issues"
      risk_level: high
      risk_rationale: "both issues challenge the author's intent"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      tested:
        - "fakeagent: focused verification"
      testing_summary: "simulated tests passed"
      title: "feat: carry-forward journey"
      body: "## Summary\ncarry-forward journey"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write carryover scenario: %v", err)
	}
	return path
}

// TestReviewCarriesUnresolvedFindingAcrossAnEmptyRereviewJourney is the
// end-user proof of the carry-forward fix, taken through the real binary, the
// real daemon, and the `axi` surface an agent or operator actually drives.
//
// The reported failure: a review round reports two ask-user findings, the
// operator asks the pipeline to fix only one of them, and the scoped re-review
// after that fix round reports nothing new. Before the fix, that empty round
// REPLACED the step's findings outright - the finding the operator never
// resolved vanished from the gate, the step completed, and the run sailed past
// review as if everything had been addressed.
//
// What this journey asserts at the operator's surface:
//   - the first gate lists both findings,
//   - after fixing only carry-1, the run parks AGAIN and the gate still lists
//     carry-2, with its original id, description, and ask-user action intact,
//   - carry-1 (the one that was actually fixed) is gone from that gate,
//   - the carried id is still a valid selector: responding with
//     `--findings carry-2` resolves it and the run then completes.
func TestReviewCarriesUnresolvedFindingAcrossAnEmptyRereviewJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: carryoverScenario(t)})
	h.CommitChange("init-carryover", "seed.txt", "seed\n", "seed carryover init")
	initWorktree := h.AddWorktree("init-carryover")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	branch := "feature/findings-carryover"
	h.CommitChange(branch, "feature.txt", "unsafe\nhardcoded-timeout\n", "add unguarded feature")
	operator := h.AddWorktree(branch)

	firstGate, err := h.RunInDir(operator, "axi", "run", "--intent", carryoverIntent)
	if err != nil {
		t.Fatalf("axi run: %v\n%s", err, firstGate)
	}
	t.Logf("--- gate 1: `no-mistakes axi run --intent %q` ---\n%s", carryoverIntent, firstGate)
	for _, want := range []string{"step: review", "status: awaiting_approval", "carry-1", "carry-2"} {
		if !strings.Contains(firstGate, want) {
			t.Fatalf("first review gate missing %q:\n%s", want, firstGate)
		}
	}

	secondGate, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "carry-1")
	if err != nil {
		t.Fatalf("axi respond fix carry-1: %v\n%s", err, secondGate)
	}
	t.Logf("--- gate 2: `no-mistakes axi respond --action fix --findings carry-1` ---\n%s", secondGate)

	// The regression: an empty re-review round must not clear the finding the
	// operator never resolved. The run has to park again on carry-2, not
	// complete.
	// "outcome:" also appears inside the help text, so the completion check is
	// on the rendered outcome value.
	if strings.Contains(secondGate, "outcome: passed") {
		t.Fatalf("run completed past review with carry-2 unresolved (findings vanished on the empty re-review round):\n%s", secondGate)
	}
	for _, want := range []string{"step: review", "status: fix_review", "carry-2", "hardcoded timeout should be configurable", "ask-user"} {
		if !strings.Contains(secondGate, want) {
			t.Fatalf("post-fix gate missing %q:\n%s", want, secondGate)
		}
	}
	// The finding the operator DID select is resolved and must not be re-parked.
	if strings.Contains(secondGate, "carry-1") {
		t.Fatalf("post-fix gate still lists the resolved finding carry-1:\n%s", secondGate)
	}

	// The same carried finding must also read as still outstanding on the
	// operator's reporting surface: while the gate is parked on carry-2 the
	// run has fixed exactly one of the two findings it reported, not both.
	statsOut, err := h.RunInDir(operator, "stats")
	if err != nil {
		t.Fatalf("no-mistakes stats: %v\n%s", err, statsOut)
	}
	t.Logf("--- `no-mistakes stats` while parked on the carried finding ---\n%s", statsOut)
	// The dashboard is column-aligned, so compare on collapsed whitespace.
	statsFields := strings.Join(strings.Fields(statsOut), " ")
	for _, want := range []string{"Reported 2", "Fixed 50%", "review 1"} {
		if !strings.Contains(statsFields, want) {
			t.Errorf("stats does not report the carried finding as still outstanding (want %q):\n%s", want, statsOut)
		}
	}

	// The carried finding keeps its identity, so the id the operator was shown
	// is still the id that selects it. If it were not, the fix response would
	// select nothing and the step would park again instead of completing.
	final, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "carry-2")
	if err != nil {
		t.Fatalf("axi respond fix carry-2: %v\n%s", err, final)
	}
	t.Logf("--- final: `no-mistakes axi respond --action fix --findings carry-2` ---\n%s", final)
	if !strings.Contains(final, "outcome: passed") {
		t.Fatalf("run did not complete after resolving the carried finding:\n%s", final)
	}
}
