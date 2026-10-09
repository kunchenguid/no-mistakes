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

// cleanCatchAllAction is the "everything else is fine" tail every scenario in
// this file ends with: the test, document, lint and push turns.
const cleanCatchAllAction = `  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      risk_scope: source-or-external
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      artifacts: []
      title: "feat: fakeagent change"
      body: "## Summary\nfakeagent canned PR body"
`

func writeScenario(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body+cleanCatchAllAction), 0o644); err != nil {
		t.Fatalf("write scenario %s: %v", name, err)
	}
	return path
}

// answerFollowHarness stands the product up with the review conversation turned
// on in the trusted default-branch config and a short question-park grace, and
// leaves a feature branch ready to run.
func answerFollowHarness(t *testing.T, scenario string) (*Harness, string) {
	t.Helper()
	h := NewHarness(t, SetupOpts{
		Agent:             "claude",
		Scenario:          scenario,
		GlobalConfigExtra: "gate_reconcile_timeout: \"2s\"",
	})
	pushMainRepoConfig(t, h, trustedRepoConfigWithReviewConversation)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	branch := "feature/answer-follow"
	h.CommitChange(branch, "feature.txt", "flag = true\n", "add the feature flag")
	return h, branch
}

func question(id, text string) string {
	return `      - '{"id":"` + id + `","kind":"question","question":"` + text +
		`","options":["yes","no"],"weight":"major","file":"feature.txt","line":1,"area":"config loader"}'`
}

// An answer that leaves a question open must return at once, exactly as it
// always did, and the answer that closes the last one must follow the run -
// including when the finalize turn asks something new, which is the park the
// grace has to tell apart from the one this answer just released.
func TestAxiAnswerPartialReturnsAtOnceAndTheClosingAnswerReturnsAGenuineReAsk(t *testing.T) {
	scenario := writeScenario(t, "two-questions.yaml", `actions:
  - match: "ccc-three"
    text: "every question is settled"
    structured:
      findings: []
      summary: "settled"
      risk_level: low
      risk_rationale: "answers settled it"
      risk_scope: source-or-external
  - match: "Answers to the questions you asked in this pass"
    text: "the answers raised one more question"
    ask_questions:
`+question("q3", "Which config file should own the default?")+`
    structured:
      findings: []
      summary: "one new question"
      risk_level: medium
      risk_rationale: "a new question is open"
      risk_scope: source-or-external
  - match: "Review the code changes and return structured findings"
    text: "asked two questions and kept reviewing"
    ask_questions:
`+question("q1", "Should the flag default to off?")+`
`+question("q2", "Should the flag be documented?")+`
    structured:
      findings: []
      summary: "two questions open"
      risk_level: medium
      risk_rationale: "questions are open"
      risk_scope: source-or-external
`)
	h, branch := answerFollowHarness(t, scenario)

	runOut, err := h.Run("axi", "run", "--intent", "wire the feature flag into the config loader")
	if err != nil {
		t.Fatalf("axi run: %v\n%s", err, runOut)
	}
	if !strings.Contains(runOut, "question-q1") || !strings.Contains(runOut, "question-q2") {
		t.Fatalf("axi run did not park on both questions:\n%s", runOut)
	}

	// A partial answer changes nothing about the run, so it must not read it
	// or block on it.
	started := time.Now()
	partial, err := h.Run("axi", "answer", "--question", "q1", "--answer", "aaa-one")
	if err != nil {
		t.Fatalf("axi answer q1: %v\n%s", err, partial)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("a partial answer blocked for %s; it must return at once:\n%s", elapsed, partial)
	}
	if !strings.Contains(partial, "open_questions: 1") {
		t.Fatalf("partial answer did not report the question still open:\n%s", partial)
	}
	for _, unwanted := range []string{"outcome:", "gate:", "steps["} {
		if strings.Contains(partial, unwanted) {
			t.Fatalf("partial answer followed the run (%q present):\n%s", unwanted, partial)
		}
	}
	t.Logf("axi answer (one question still open):\n%s", partial)

	// The closing answer sets the run moving. The park it released still reads
	// as parked on q1/q2 for a moment, and the finalize turn then parks on a
	// genuinely new question: the one that must come back is q3.
	closing, err := h.Run("axi", "answer", "--question", "q2", "--answer", "bbb-two", "--by", "captain")
	if err != nil {
		t.Fatalf("axi answer q2: %v\n%s", err, closing)
	}
	if !strings.Contains(closing, "gate:") || !strings.Contains(closing, "question-q3") {
		t.Fatalf("the closing answer did not return the finalize turn's new question as the next gate:\n%s", closing)
	}
	if strings.Contains(closing, "question-q1") || strings.Contains(closing, "question-q2") {
		t.Fatalf("the closing answer returned the park it had just released:\n%s", closing)
	}
	t.Logf("axi answer (closed the last question; the re-ask came back as the next gate):\n%s", closing)

	final, err := h.Run("axi", "answer", "--question", "q3", "--answer", "ccc-three")
	if err != nil {
		t.Fatalf("axi answer q3: %v\n%s", err, final)
	}
	if !strings.Contains(final, "outcome: passed") {
		t.Fatalf("the last answer did not follow the run to its outcome:\n%s", final)
	}
	t.Logf("axi answer (settled the last question; followed to the outcome):\n%s", final)

	if run := h.WaitForRun(branch, 60*time.Second); run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed", run.Status)
	}
}

// A question asked by the rereview inside a fix round parks as fix_review, not
// awaiting_approval. That park must be recognised as the one the answer
// released, or the answer hands it straight back as the next decision point.
func TestAxiAnswerFollowsAQuestionParkedInsideAFixRound(t *testing.T) {
	scenario := writeScenario(t, "fix-round-question.yaml", `actions:
  - match: "Answers to the questions you asked in this pass"
    text: "the answer certifies the fix"
    structured:
      findings: []
      summary: "the fix holds"
      risk_level: low
      risk_rationale: "answer settled the remaining doubt"
      risk_scope: source-or-external
  - match: "Fix-round provenance:"
    text: "one question before I can certify the fix"
    ask_questions:
`+question("q1", "Is the fixed default the one you want shipped?")+`
    structured:
      findings: []
      summary: "one question open on the fix"
      risk_level: medium
      risk_rationale: "a question is open"
      risk_scope: source-or-external
  - match: "Investigate previous review findings"
    text: "applied the fix"
    edits:
      - path: feature.txt
        new: "flag = false\n"
    structured:
      findings: []
      summary: "fix applied"
  - match: "Review the code changes and return structured findings"
    text: "one finding"
    structured:
      findings:
        - id: "needs-work"
          severity: warning
          file: "feature.txt"
          line: 1
          description: "the flag ships enabled for existing installations"
          action: ask-user
      summary: "one finding"
      risk_level: medium
      risk_rationale: "a finding is open"
      risk_scope: source-or-external
`)
	h, branch := answerFollowHarness(t, scenario)

	runOut, err := h.Run("axi", "run", "--intent", "wire the feature flag into the config loader")
	if err != nil || !strings.Contains(runOut, "needs-work") {
		t.Fatalf("axi run did not park on the finding: %v\n%s", err, runOut)
	}
	fixOut, err := h.Run("axi", "respond", "--action", "fix", "--findings", "needs-work")
	if err != nil {
		t.Fatalf("axi respond --action fix: %v\n%s", err, fixOut)
	}
	if !strings.Contains(fixOut, "question-q1") {
		t.Fatalf("the rereview inside the fix round did not park on its question:\n%s", fixOut)
	}
	parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusFixReview, 60*time.Second)
	if parked == nil {
		t.Fatal("the question park inside the fix round is not fix_review")
	}
	t.Logf("the fix round's rereview parked as fix_review on question-q1:\n%s", fixOut)

	answerOut, err := h.Run("axi", "answer", "--question", "q1", "--answer", "yes", "--by", "captain")
	if err != nil {
		t.Fatalf("axi answer: %v\n%s", err, answerOut)
	}
	if strings.Contains(answerOut, "question-q1") {
		t.Fatalf("the answer returned the fix_review park it had just released:\n%s", answerOut)
	}
	if !strings.Contains(answerOut, "outcome: passed") {
		t.Fatalf("the answer did not follow the fix round's run to its outcome:\n%s", answerOut)
	}
	t.Logf("axi answer at a fix_review question park:\n%s", answerOut)
}

// A --wait that elapses after the answer is recorded must say so, and must send
// the caller back with the non-mutating `axi run` rather than another answer.
func TestAxiAnswerWaitElapsedReattachesWithAxiRun(t *testing.T) {
	scenario := writeScenario(t, "slow-finalize.yaml", `actions:
  - match: "Answers to the questions you asked in this pass"
    text: "taking my time"
    delay_ms: 20000
    structured:
      findings: []
      summary: "settled"
      risk_level: low
      risk_rationale: "answer settled it"
      risk_scope: source-or-external
  - match: "Review the code changes and return structured findings"
    text: "asked one question"
    ask_questions:
`+question("q1", "Should the flag default to off?")+`
    structured:
      findings: []
      summary: "one question open"
      risk_level: medium
      risk_rationale: "a question is open"
      risk_scope: source-or-external
`)
	h, _ := answerFollowHarness(t, scenario)

	runOut, err := h.Run("axi", "run", "--intent", "wire the feature flag into the config loader")
	if err != nil || !strings.Contains(runOut, "question-q1") {
		t.Fatalf("axi run did not park on the question: %v\n%s", err, runOut)
	}

	out, err := h.Run("axi", "answer", "--question", "q1", "--answer", "yes", "--wait", "3s")
	if err == nil {
		t.Fatalf("axi answer with an elapsed wait succeeded, want the bounded-hold error:\n%s", out)
	}
	if !strings.Contains(out, "wait of 3s elapsed while driving the run") {
		t.Fatalf("the elapsed wait was not reported as one:\n%s", out)
	}
	if !strings.Contains(out, "Re-run `no-mistakes axi run`") {
		t.Fatalf("the elapsed wait does not reattach with the non-mutating command:\n%s", out)
	}
	if strings.Contains(out, "Re-run `no-mistakes axi answer") {
		t.Fatalf("the elapsed wait told the caller to answer again:\n%s", out)
	}
	if strings.Contains(out, "not found") {
		t.Fatalf("the elapsed wait was reported as a missing run:\n%s", out)
	}
	t.Logf("axi answer whose --wait elapsed after the answer was recorded:\n%s", out)
}
