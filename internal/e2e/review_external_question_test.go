//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An external fact may be settled, but the reviewer's open questions require
// answers even when the next review gives those questions new finding IDs.
func TestExternalSettlementCannotCompleteReviewWithUnansweredQuestions(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "question-external.yaml")
	writeScenario := func(question string, ordinary bool) {
		t.Helper()
		finding := "[]"
		if ordinary {
			finding = `[{"id":"question-fact","file":"feature.txt","line":1,"severity":"warning","action":"auto-fix","description":"external fact"}]`
		}
		content := `actions:
  - match: "Review the code changes and return structured findings"
    ask_questions:
      - '{"id":"` + question + `","kind":"question","question":"Keep the feature?","options":["keep","remove"],"weight":"major","file":"feature.txt","line":1}'
    structured:
      findings: ` + finding + `
      summary: "review question open"
      risk_level: medium
      risk_rationale: "answer needed"
      risk_scope: source-or-external
      reviewed_paths: [feature.txt]
`
		if err := os.WriteFile(scenario, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeScenario("q1", true)
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	pushMainRepoConfig(t, h, trustedRepoConfigWithReviewConversation)
	h.CommitChange("init-question-external", "seed.txt", "seed\n", "seed question lab")
	if out, err := h.RunInDir(h.AddWorktree("init-question-external"), "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	branch := "feature/question-external"
	h.CommitChange(branch, "feature.txt", "feature\n", "review external fact")
	work := h.AddWorktree(branch)
	run := func(args ...string) string {
		t.Helper()
		out, err := h.RunInDir(work, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	gate := run("axi", "run", "--skip", "test,document,lint,push,pr,ci", "--intent", "review an external fact and answer questions")
	if !strings.Contains(gate, "question-q1") || !strings.Contains(gate, "fact") {
		t.Fatalf("expected question and external fact at first gate:\n%s", gate)
	}
	refusal, err := h.RunInDir(work, "axi", "respond", "--action", "resolved_external", "--findings", "question-q1", "--reason", "verified externally")
	if err == nil || !strings.Contains(refusal, "review question") {
		t.Fatalf("external response must refuse an unanswered question: %v\n%s", err, refusal)
	}
	// The ordinary external fact remains eligible. A rereview asks a new
	// question while the first question is still open on disk.
	writeScenario("q2", false)
	gate = run("axi", "respond", "--action", "resolved_external", "--findings", "question-fact", "--reason", "verified externally")
	if !strings.Contains(gate, "question-q1") || !strings.Contains(gate, "question-q2") || strings.Contains(gate, "outcome: passed") {
		t.Fatalf("rereview lost an unanswered or reissued question:\n%s", gate)
	}
	// A second external settlement cannot use the new question's ID either.
	refusal, err = h.RunInDir(work, "axi", "respond", "--action", "resolved_external", "--findings", "question-q2", "--reason", "verified externally")
	if err == nil || !strings.Contains(refusal, "review question") {
		t.Fatalf("reissued question accepted as an external fact: %v\n%s", err, refusal)
	}
}
