//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// documentTurnMarker identifies a document-pass invocation by the real prompt
// the running binary issues: the task's "report only what you could not
// resolve" instruction immediately followed by the Context block's branch
// line. Review, test, and intent turns carry the branch line too but never
// this instruction, so the marker cannot alias another step's turn.
const documentTurnMarker = "report only what you could not resolve.\n\nContext:\n- branch: "

// documentSummaryContractScenario scripts the fake agent with one document
// response per scenario branch plus a clean catch-all for every other turn
// (review, test, intent). The branches are:
//
//   - document-noop-empty-summary: zero findings, present-but-empty summary,
//     no edits - the legitimate no-op pass that used to be refused with
//     "missing summary" (run 01M4NVPRT28FA9PP4CB6Z7G1EH).
//   - document-findings-empty-summary: an unresolved finding with an empty
//     summary - summary still required.
//   - document-absent-summary-key: no summary field at all - a shape
//     violation in every case.
//   - document-edits-empty-summary: the agent leaves an uncommitted edit -
//     not a no-op, summary required, edit still committed by the pipeline.
//   - document-self-commit-empty-summary: the agent commits its own edit, so
//     the worktree is clean but the head moved - head movement counts as
//     changes, summary required, the agent's commit stays.
func documentSummaryContractScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "document-summary-contract-scenario.yaml")
	content := `actions:
  - match: "report only what you could not resolve.\n\nContext:\n- branch: document-self-commit-empty-summary"
    text: "self-committed a documentation edit"
    edits:
      - path: "README.md"
        old: "# e2e\n"
        new: "# e2e self committed\n"
    stage: ["README.md"]
    git:
      - ["commit", "-m", "docs: self-commit the README edit"]
    structured:
      findings: []
      summary: ""
  - match: "report only what you could not resolve.\n\nContext:\n- branch: document-edits-empty-summary"
    text: "edited documentation without a summary"
    edits:
      - path: "README.md"
        old: "# e2e\n"
        new: "# e2e edited\n"
    structured:
      findings: []
      summary: ""
  - match: "report only what you could not resolve.\n\nContext:\n- branch: document-findings-empty-summary"
    text: "one unresolved documentation gap"
    structured:
      findings:
        - severity: warning
          description: "README no longer matches the change"
          action: ask-user
          category: documentation
      summary: ""
  - match: "report only what you could not resolve.\n\nContext:\n- branch: document-absent-summary-key"
    text: " "
    structured:
      findings: []
  - match: "report only what you could not resolve.\n\nContext:\n- branch: document-noop-empty-summary"
    text: " "
    structured:
      findings: []
      summary: ""
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      risk_scope: source-or-external
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      artifacts: []
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      title: "feat: document summary contract"
      body: "## Summary\ndocument summary contract journey"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write document summary contract scenario: %v", err)
	}
	return path
}

// TestDocumentAnalyzerSummaryContractOnRealBinary drives the real no-mistakes
// binary end to end (init -> git push -> daemon pipeline) and pins the
// document step's summary contract: a legitimate no-op pass succeeds where it
// used to be refused with "validate document analyzer findings: missing
// summary", while a summary is still required whenever the pass reports
// unresolved findings, omits the summary field, or actually changed files -
// including an agent that committed its own edits and left a clean worktree.
func TestDocumentAnalyzerSummaryContractOnRealBinary(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: documentSummaryContractScenario(t)})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	documentInvocations := func(branch string) int {
		count := 0
		for _, inv := range h.AgentInvocations() {
			if strings.Contains(inv.Prompt, documentTurnMarker+branch) {
				count++
			}
		}
		return count
	}
	documentStepOf := func(run *ipc.RunInfo) ipc.StepResultInfo {
		t.Helper()
		step, ok := findStep(run.Steps, types.StepDocument)
		if !ok {
			t.Fatalf("run %s has no document step: %+v", run.ID, run.Steps)
		}
		return step
	}
	// documentStepLog reads the failed run's document step log from the
	// daemon's durable per-run log directory, so the assertions below are
	// backed by what the step itself recorded rather than inference.
	documentStepLog := func(runID string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(h.NMHome, "logs", runID, "document.log"))
		if err != nil {
			t.Fatalf("read document step log for run %s: %v", runID, err)
		}
		return string(raw)
	}
	assertMissingSummaryFailure := func(run *ipc.RunInfo, label string) *ipc.RunInfo {
		t.Helper()
		if run.Status != types.RunFailed {
			t.Fatalf("%s: run status = %s, want failed (error: %s)", label, run.Status, deref(run.Error))
		}
		step := documentStepOf(run)
		if step.Status != types.StepStatusFailed {
			t.Fatalf("%s: document step status = %s, want failed", label, step.Status)
		}
		if step.Error == nil || !strings.Contains(*step.Error, "validate document analyzer findings: missing summary") {
			t.Fatalf("%s: document step error = %v, want it to name the missing summary", label, step.Error)
		}
		return run
	}

	// A legitimate no-op pass succeeds: the document agent runs, resolves
	// nothing to change, and returns {"findings":[],"summary":""}; the run
	// completes instead of being refused with "missing summary".
	noopHead := h.CommitChange("document-noop-empty-summary", "noop-change.txt", "documentation noop\n", "add documentation noop change")
	h.PushToGate("document-noop-empty-summary")
	noopRun := h.WaitForRun("document-noop-empty-summary", 180*time.Second)
	if noopRun.Status != types.RunCompleted {
		t.Fatalf("no-op document pass: run status = %s, want completed (error: %s)", noopRun.Status, deref(noopRun.Error))
	}
	noopDoc := documentStepOf(noopRun)
	if noopDoc.Status != types.StepStatusCompleted {
		t.Fatalf("no-op document pass: document step status = %s, want completed (error: %s)", noopDoc.Status, deref(noopDoc.Error))
	}
	if noopRun.HeadSHA != noopHead {
		t.Fatalf("no-op document pass: run head = %s, want the unchanged submitted head %s", noopRun.HeadSHA, noopHead)
	}
	if n := documentInvocations("document-noop-empty-summary"); n == 0 {
		t.Fatal("no-op document pass: the document agent never ran; the pass must analyze, not skip")
	}
	t.Logf("no-op pass: document step log:\n%s", documentStepLog(noopRun.ID))

	// Unresolved findings with an empty summary still fail: a summary is
	// required whenever the agent reports what remains.
	h.CommitChange("document-findings-empty-summary", "findings-change.txt", "findings\n", "add findings change")
	h.PushToGate("document-findings-empty-summary")
	findingsRun := assertMissingSummaryFailure(h.WaitForRun("document-findings-empty-summary", 180*time.Second), "findings with empty summary")
	if n := documentInvocations("document-findings-empty-summary"); n == 0 {
		t.Fatal("findings with empty summary: the document agent never ran")
	}
	t.Logf("findings pass: run %s failed as required", findingsRun.ID)

	// An absent summary field stays a shape violation in every case.
	h.CommitChange("document-absent-summary-key", "absent-key-change.txt", "absent key\n", "add absent key change")
	h.PushToGate("document-absent-summary-key")
	assertMissingSummaryFailure(h.WaitForRun("document-absent-summary-key", 180*time.Second), "absent summary key")

	// Real edits with an empty summary are not a no-op: the step refuses the
	// pass and the pipeline still commits what the agent edited.
	editsHead := h.CommitChange("document-edits-empty-summary", "edits-change.txt", "edits\n", "add edits change")
	h.PushToGate("document-edits-empty-summary")
	editsRun := assertMissingSummaryFailure(h.WaitForRun("document-edits-empty-summary", 180*time.Second), "edits with empty summary")
	if editsRun.HeadSHA == editsHead {
		t.Fatal("edits with empty summary: expected the agent's edit to be committed despite the missing summary")
	}
	editsLog := documentStepLog(editsRun.ID)
	if !strings.Contains(editsLog, "committed agent fixes") {
		t.Fatalf("edits with empty summary: expected the pipeline to commit the agent's edit, step log:\n%s", editsLog)
	}
	t.Logf("edits pass: document step log:\n%s", editsLog)

	// An agent that commits its own edits leaves a clean worktree, but the
	// head moved: head movement counts as changes, so the pass is refused,
	// the recorded head follows the agent's commit, and the pipeline never
	// staged anything itself.
	selfHead := h.CommitChange("document-self-commit-empty-summary", "self-commit-change.txt", "self commit\n", "add self commit change")
	h.PushToGate("document-self-commit-empty-summary")
	selfRun := assertMissingSummaryFailure(h.WaitForRun("document-self-commit-empty-summary", 180*time.Second), "self-committed edits with empty summary")
	if selfRun.HeadSHA == selfHead {
		t.Fatal("self-committed edits with empty summary: expected the recorded head to follow the agent's own commit")
	}
	selfLog := documentStepLog(selfRun.ID)
	if !strings.Contains(selfLog, "no agent changes to commit") {
		t.Fatalf("self-committed edits with empty summary: expected a clean worktree (the agent committed its own edit), step log:\n%s", selfLog)
	}
	if !strings.Contains(selfLog, "validate document analyzer findings: missing summary") {
		t.Fatalf("self-committed edits with empty summary: expected the step log to record the refusal, step log:\n%s", selfLog)
	}
	t.Logf("self-commit pass: document step log:\n%s", selfLog)
}

// TestDocumentNoOpPassOnCodexBinary repeats the legitimate no-op pass through
// the codex wire format - the backend whose structured `{"findings":[],
// "summary":""}` success was refused four times in a row on the recorded
// county-data run. The codex adapter parses structured output out of the
// agent_message text and hands it to the same step validator, so the pass
// must succeed there too.
func TestDocumentNoOpPassOnCodexBinary(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: documentSummaryContractScenario(t)})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	head := h.CommitChange("document-noop-empty-summary", "noop-change.txt", "documentation noop\n", "add documentation noop change")
	h.PushToGate("document-noop-empty-summary")
	run := h.WaitForRun("document-noop-empty-summary", 180*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("codex no-op document pass: run status = %s, want completed (error: %s)", run.Status, deref(run.Error))
	}
	step, ok := findStep(run.Steps, types.StepDocument)
	if !ok {
		t.Fatal("codex no-op document pass: run has no document step")
	}
	if step.Status != types.StepStatusCompleted {
		t.Fatalf("codex no-op document pass: document step status = %s, want completed (error: %s)", step.Status, deref(step.Error))
	}
	if run.HeadSHA != head {
		t.Fatalf("codex no-op document pass: run head = %s, want the unchanged submitted head %s", run.HeadSHA, head)
	}
	documentRan := false
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, documentTurnMarker+"document-noop-empty-summary") {
			documentRan = true
		}
	}
	if !documentRan {
		t.Fatal("codex no-op document pass: the document agent never ran; the pass must analyze, not skip")
	}
}
