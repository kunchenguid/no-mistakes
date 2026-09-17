//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The question the fake reviewer asks mid-pass, and the answer the operator
// sends back with `no-mistakes axi answer`.
const (
	conversationQuestionText = "Should the new flag default to off for existing installations?"
	conversationAnswerText   = "default off"
	conversationAnsweredBy   = "captain"
)

// trustedRepoConfigWithReviewConversation is the .no-mistakes.yaml a maintainer
// commits to the default branch to turn the conversation on. It is trusted-only,
// so this is the only place it can come from.
const trustedRepoConfigWithReviewConversation = `ignore_patterns:
  - 'vendor/**'
allow_repo_commands: true
review:
  conversation: true
`

// reviewConversationScenario drives a reviewer that ASKS while it works: the
// first review turn appends one question to the run's own questions.ndjson
// (through the channel the prompt names, exactly as a real reviewer would) and
// returns a finding that depends on the answer. The finalize turn - recognised
// by the answers section the step appends to the whole review prompt - returns
// a clean pass, so the run can only finish if the answer really reached the
// reviewer.
//
// The finalize action is listed FIRST because the scenario matcher takes the
// first matching substring, and the finalize prompt contains the review
// prompt's own marker too.
func reviewConversationScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "review-conversation-scenario.yaml")
	content := `actions:
  - match: "Answers to the questions you asked in this pass"
    text: "finished the pass with the operator's answer"
    structured:
      findings: []
      summary: "the open question is settled; nothing blocking"
      risk_level: low
      risk_rationale: "answered question resolved the only concern"
      risk_scope: source-or-external
  - match: "Review the code changes and return structured findings"
    text: "asked one question and kept reviewing"
    ask_questions:
      - '{"id":"q1","kind":"question","question":"` + conversationQuestionText + `","options":["default off","default on"],"weight":"major","file":"feature.txt","line":1,"area":"config loader"}'
    structured:
      findings:
        - id: "review-pending"
          severity: warning
          file: "feature.txt"
          line: 1
          description: "PENDING ANSWER (q1): the default this ships with depends on the answer"
          action: ask-user
      summary: "one question open"
      risk_level: medium
      risk_rationale: "a question is open"
      risk_scope: source-or-external
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
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write review conversation scenario: %v", err)
	}
	return path
}

// TestReviewConversationJourney is the end-to-end proof of the review
// conversation as an operator experiences it: a maintainer turns
// `review.conversation` on in the trusted default-branch config, the reviewer
// asks a question while it reviews, the run parks for a human answer,
// `no-mistakes axi answer` settles it, and the same reviewer finishes its pass.
//
// It also drives the two boundaries the feature is only safe with. `--yes` must
// stand aside at an open question instead of handing it to the fixer, and
// `axi respond --action answer` must not exist - an answer is not a verdict.
func TestReviewConversationJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: reviewConversationScenario(t)})
	pushMainRepoConfig(t, h, trustedRepoConfigWithReviewConversation)

	h.CommitChange("init-conversation", "seed.txt", "seed\n", "seed for the conversation journey")
	initWorktree := h.AddWorktree("init-conversation")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	branch := "feature/review-conversation"
	h.CommitChange(branch, "feature.txt", "flag = true\n", "add the feature flag")
	fw := h.AddWorktree(branch)

	// --yes is the adversarial half: it auto-resolves every eligible gate, and
	// the gate it meets here carries an ordinary ask-user finding alongside the
	// question. It must still stand aside and say how to answer, rather than
	// selecting the question as work for the fixer.
	driveOut, err := h.RunInDir(fw, "axi", "run", "--yes", "--intent", "wire the feature flag into the config loader")
	if err != nil {
		t.Fatalf("axi run --yes (expected to stand aside at the question, exit 0): %v\n%s", err, driveOut)
	}
	if !strings.Contains(driveOut, "an open review question needs an explicit answer") {
		t.Errorf("axi run --yes did not stand aside at the open review question:\n%s", driveOut)
	}

	gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if gated == nil {
		t.Fatal("review step never parked for an answer")
	}
	runID := gated.ID
	t.Logf("axi run --yes output at the question gate:\n%s", driveOut)

	// The reviewer really used the channel the prompt named.
	prompt := reviewPrompt(t, h)
	if !strings.Contains(prompt, "You have a question channel:") {
		t.Fatalf("review prompt carries no question channel:\n%s", promptTail(prompt))
	}
	convDir := reviewqa.Dir(filepath.Join(h.NMHome, "evidence", runID))
	questionsPath := filepath.Join(convDir, reviewqa.QuestionsFile)
	questions, err := os.ReadFile(questionsPath)
	if err != nil {
		t.Fatalf("read %s: %v", questionsPath, err)
	}
	if !strings.Contains(string(questions), conversationQuestionText) {
		t.Fatalf("questions.ndjson does not carry the reviewer's question:\n%s", questions)
	}

	// The operator sees the open question as an answerable row, with its id,
	// its options, and the command that settles it.
	statusOut, err := h.RunInDir(fw, "axi", "status")
	if err != nil {
		t.Fatalf("axi status (parked on a question): %v\n%s", err, statusOut)
	}
	for _, want := range []string{
		"question-q1",
		conversationQuestionText,
		"default off",
		"no-mistakes axi answer --question",
	} {
		if !strings.Contains(statusOut, want) {
			t.Errorf("axi status at the question gate does not show %q:\n%s", want, statusOut)
		}
	}
	t.Logf("axi status at the question gate:\n%s", statusOut)

	// An answer is not a verdict: `respond` has no answer action at all.
	respondOut, err := h.RunInDir(fw, "axi", "respond", "--action", "answer")
	if err == nil {
		t.Errorf("axi respond --action answer succeeded, want a refusal:\n%s", respondOut)
	}
	if !strings.Contains(respondOut, "unknown action") || !strings.Contains(respondOut, "answer") {
		t.Errorf("axi respond --action answer did not refuse by name:\n%s", respondOut)
	}
	if !strings.Contains(respondOut, "Valid actions: approve, fix, skip") {
		t.Errorf("axi respond refusal does not list the actions that do exist:\n%s", respondOut)
	}

	// The answer settles the question and releases the gate by resuming the
	// reviewer - not by approving or fixing anything.
	answerOut, err := h.RunInDir(fw, "axi", "answer",
		"--question", "q1", "--answer", conversationAnswerText, "--by", conversationAnsweredBy)
	if err != nil {
		t.Fatalf("axi answer: %v\n%s", err, answerOut)
	}
	for _, want := range []string{"answered: true", "open_questions: 0", "reviewer_resumed: true"} {
		if !strings.Contains(answerOut, want) {
			t.Errorf("axi answer output missing %q:\n%s", want, answerOut)
		}
	}
	t.Logf("axi answer output:\n%s", answerOut)

	answers, err := os.ReadFile(filepath.Join(convDir, reviewqa.AnswersFile))
	if err != nil {
		t.Fatalf("read answers.ndjson: %v", err)
	}
	for _, want := range []string{conversationAnswerText, conversationAnsweredBy} {
		if !strings.Contains(string(answers), want) {
			t.Errorf("answers.ndjson does not carry %q:\n%s", want, answers)
		}
	}

	completed := h.WaitForRun(branch, 120*time.Second)
	if completed.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", completed.Status, deref(completed.Error))
	}

	// The finalize turn is the same reviewer being handed the answer, not a
	// fresh round of work: its prompt carries the answers section verbatim.
	finalize := ""
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, "Answers to the questions you asked in this pass") {
			finalize = inv.Prompt
		}
	}
	if finalize == "" {
		t.Fatal("no finalize review turn received the answers")
	}
	if !strings.Contains(finalize, conversationAnswerText) {
		t.Errorf("finalize review prompt does not carry the operator's answer:\n%s", promptTail(finalize))
	}
	// No fixer ever ran on the question: --yes stood aside, and the answer path
	// never reaches the fix agent.
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, "Investigate previous review findings") {
			t.Errorf("a fix round ran over the review question:\n%s", promptTail(inv.Prompt))
		}
	}
}

// TestReviewConversationOffRefusesAnAnswer is the off-by-default half. A
// repository that never asked for the conversation gets today's review: the
// prompt carries no question channel, the run creates no conversation
// directory, and `axi answer` refuses and names the setting that would accept
// an answer.
func TestReviewConversationOffRefusesAnAnswer(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t)})

	h.CommitChange("init-conversation-off", "seed.txt", "seed\n", "seed for the off journey")
	initWorktree := h.AddWorktree("init-conversation-off")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	branch := "feature/conversation-off"
	h.CommitChange(branch, "feature.txt", "flag = true\n", "add the feature flag")
	fw := h.AddWorktree(branch)

	if out, err := h.RunInDir(fw, "axi", "run", "--intent", "wire the feature flag into the config loader"); err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if gated == nil {
		t.Fatal("review step never parked")
	}

	prompt := reviewPrompt(t, h)
	for _, absent := range []string{"You have a question channel:", "Asking questions while you work:"} {
		if strings.Contains(prompt, absent) {
			t.Errorf("review prompt carries %q with review.conversation off:\n%s", absent, promptTail(prompt))
		}
	}
	convDir := reviewqa.Dir(filepath.Join(h.NMHome, "evidence", gated.ID))
	if _, err := os.Stat(convDir); !os.IsNotExist(err) {
		t.Errorf("conversation directory %s exists with review.conversation off (stat err=%v)", convDir, err)
	}

	out, err := h.RunInDir(fw, "axi", "answer", "--question", "q1", "--answer", "default off")
	if err == nil {
		t.Errorf("axi answer succeeded with the conversation off, want a refusal:\n%s", out)
	}
	if !strings.Contains(out, "review.conversation") {
		t.Errorf("axi answer refusal does not name the setting that would accept an answer:\n%s", out)
	}
	t.Logf("axi answer refusal with the conversation off:\n%s", out)

	// Nothing was written into the run's evidence directory by the refusal.
	if _, err := os.Stat(convDir); !os.IsNotExist(err) {
		t.Errorf("refused answer created %s (stat err=%v)", convDir, err)
	}

	// The gate is still the operator's to resolve, unchanged.
	if out, err := h.RunInDir(fw, "axi", "respond", "--action", "approve"); err != nil {
		t.Fatalf("axi respond approve: %v\n%s", err, out)
	}
	if completed := h.WaitForRun(branch, 120*time.Second); completed.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed", completed.Status)
	}

}

// trustedRepoConfigWithConversationAndEvidenceBranch turns both halves on: the
// review conversation, and the opt-in that copies the run's evidence directory
// onto the repository's orphan evidence branch. The conversation files live in
// that same directory, which is exactly the collision under test.
const trustedRepoConfigWithConversationAndEvidenceBranch = `ignore_patterns:
  - 'vendor/**'
allow_repo_commands: true
review:
  conversation: true
test:
  evidence:
    store_in_repo: true
    attach_media: false
`

// evidenceBranchConversationScenario asks a question during review (so the run
// really has a conversation on disk) and writes one ordinary test-evidence file
// during the test step (so the evidence branch is really published).
func evidenceBranchConversationScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evidence-branch-conversation.yaml")
	content := `actions:
  - match: "Answers to the questions you asked in this pass"
    text: "finished the pass with the operator's answer"
    structured:
      findings: []
      summary: "the open question is settled"
      risk_level: low
      risk_rationale: "answered"
      risk_scope: source-or-external
  - match: "Review the code changes and return structured findings"
    text: "asked one question and kept reviewing"
    ask_questions:
      - '{"id":"q1","kind":"question","question":"` + conversationQuestionText + `","options":["default off","default on"],"weight":"major","file":"feature.txt","line":1,"area":"config loader"}'
    structured:
      findings: []
      summary: "one question open"
      risk_level: medium
      risk_rationale: "a question is open"
      risk_scope: source-or-external
  - match: "You are validating a code change by driving the product itself."
    text: "captured one evidence file"
    write_evidence:
      - path: "run-transcript.txt"
        content: "fakeagent: the flag defaults to off\n"
    structured:
      findings: []
      summary: "all tests passed"
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "run-transcript.txt"
          reason: ""
      verdict: go
      artifacts:
        - label: "run transcript"
          path: "run-transcript.txt"
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
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write evidence-branch conversation scenario: %v", err)
	}
	return path
}

// TestReviewConversationNeverReachesTheEvidenceBranch is the disclosure guard,
// driven through the real pipeline against a repository that has BOTH the
// conversation and the orphan evidence branch turned on.
//
// The operator's question and answer - full text, and who answered - sit in the
// run's evidence directory, which is the directory the evidence branch
// publishes verbatim and permanently. The published branch must carry the test
// evidence and nothing of the conversation.
func TestReviewConversationNeverReachesTheEvidenceBranch(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: evidenceBranchConversationScenario(t)})

	// A GitHub-shaped origin rewritten to the local bare repo: evidence links
	// are only derivable for GitHub, and the rewrite keeps every push local.
	const originURL = "https://github.com/owner/no-mistakes.git"
	configureGitURLRewrite(t, h, originURL, h.UpstreamDir)
	if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", originURL); err != nil {
		t.Fatalf("set github-shaped origin: %v\n%s", err, out)
	}
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", filepath.Join(filepath.Dir(h.AgentLog), "gh-evidence.log"))
	t.Setenv("FAKEAGENT_GH_PARENT", "owner/no-mistakes")

	pushMainRepoConfig(t, h, trustedRepoConfigWithConversationAndEvidenceBranch)

	h.CommitChange("init-evidence-conversation", "seed.txt", "seed\n", "seed for the evidence journey")
	initWorktree := h.AddWorktree("init-evidence-conversation")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	branch := "feature/evidence-conversation"
	h.CommitChange(branch, "feature.txt", "flag = true\n", "add the feature flag")
	fw := h.AddWorktree(branch)

	if out, err := h.RunInDir(fw, "axi", "run", "--intent", "wire the feature flag into the config loader"); err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if gated == nil {
		t.Fatal("review step never parked for an answer")
	}
	runID := gated.ID

	if out, err := h.RunInDir(fw, "axi", "answer",
		"--question", "q1", "--answer", conversationAnswerText, "--by", conversationAnsweredBy); err != nil {
		t.Fatalf("axi answer: %v\n%s", err, out)
	}

	// The conversation really was on disk in the directory the evidence branch
	// publishes from, which is what makes the exclusion meaningful.
	convDir := reviewqa.Dir(filepath.Join(h.NMHome, "evidence", runID))
	if _, err := os.Stat(filepath.Join(convDir, reviewqa.QuestionsFile)); err != nil {
		t.Fatalf("the run has no conversation to exclude: %v", err)
	}

	run := h.WaitForRun(branch, 180*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, deref(run.Error))
	}

	// Read the published evidence branch out of the upstream repository the
	// pipeline actually pushed to.
	tree, err := h.runGit(t.Context(), h.UpstreamDir, "ls-tree", "-r", "--name-only", "no-mistakes/evidence")
	if err != nil {
		t.Fatalf("evidence branch was not published: %v\n%s", err, tree)
	}
	published := string(tree)
	t.Logf("published evidence branch tree:\n%s", published)
	if !strings.Contains(published, "run-transcript.txt") {
		t.Fatalf("the evidence branch carries no test evidence, so the exclusion proves nothing:\n%s", published)
	}
	for _, leaked := range []string{reviewqa.DirName + "/", reviewqa.QuestionsFile, reviewqa.AnswersFile} {
		if strings.Contains(published, leaked) {
			t.Errorf("the evidence branch carries %q from the review conversation:\n%s", leaked, published)
		}
	}

	// The deliberate, bounded copy IS published - in the PR body the pipeline
	// actually sent to the forge, inside the Pipeline section as its own group.
	ghLog := filepath.Join(filepath.Dir(h.AgentLog), "gh-evidence.log")
	prBody := ""
	for _, inv := range readGHStubInvocations(t, ghLog) {
		if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "create" && inv.Body != "" {
			prBody = inv.Body
		}
	}
	if prBody == "" {
		t.Fatal("no PR body was sent to the forge")
	}
	t.Logf("review conversation as published in the PR body:\n%s", prConversationSection(prBody))
	for _, want := range []string{
		"### Review conversation",
		conversationQuestionText,
		conversationAnswerText,
		conversationAnsweredBy,
	} {
		if !strings.Contains(prBody, want) {
			t.Errorf("PR body does not record %q:\n%s", want, prBody)
		}
	}

	// And no blob on that branch carries the conversation's text, whatever it
	// was named.
	blobs, err := h.runGit(t.Context(), h.UpstreamDir, "grep", "-I", "-l", "-e", conversationAnswerText, "-e", conversationQuestionText, "-e", conversationAnsweredBy, "no-mistakes/evidence")
	if err == nil {
		t.Errorf("the evidence branch carries the conversation's text:\n%s", blobs)
	}
}

// prConversationSection returns the PR body's review-conversation group, for a
// reviewer reading the test log.
func prConversationSection(body string) string {
	i := strings.Index(body, "### Review conversation")
	if i < 0 {
		return "(absent)"
	}
	rest := body[i:]
	if j := strings.Index(rest[1:], "\n### "); j >= 0 {
		rest = rest[:j+1]
	}
	return rest
}

// TestReviewConversationTUIYoloLeavesTheQuestionOpen is the second auto-resolve
// path. `axi --yes` and the TUI's yolo mode are the two places that answer a
// gate without a human, and a carve-out on only one of them is what let the
// fixer receive a question as work. This drives the real TUI through a
// pseudo-terminal, turns yolo on at a gate carrying an open question, and
// requires the gate to still be parked afterwards.
func TestReviewConversationTUIYoloLeavesTheQuestionOpen(t *testing.T) {
	// The driver needs a pty to make the TUI behave as it does for an
	// operator, so it is a python3 script using pty.fork(): Unix-only, and on
	// a machine with no python3 this is a missing tool rather than a product
	// regression. Skipping says which, instead of failing the suite in a way
	// that reads like a defect.
	if runtime.GOOS == "windows" {
		t.Skip("the TUI driver uses pty.fork(), which is Unix-only")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; it drives the TUI through a pty")
	}
	driver, err := filepath.Abs(filepath.Join("testdata", "tui_yolo_driver.py"))
	if err != nil {
		t.Fatalf("resolve tui driver: %v", err)
	}
	if _, err := os.Stat(driver); err != nil {
		t.Fatalf("tui driver missing: %v", err)
	}

	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: reviewConversationScenario(t)})
	pushMainRepoConfig(t, h, trustedRepoConfigWithReviewConversation)

	h.CommitChange("init-tui-conversation", "seed.txt", "seed\n", "seed for the tui journey")
	initWorktree := h.AddWorktree("init-tui-conversation")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	branch := "feature/tui-conversation"
	h.CommitChange(branch, "feature.txt", "flag = true\n", "add the feature flag")
	fw := h.AddWorktree(branch)

	if out, err := h.RunInDir(fw, "axi", "run", "--intent", "wire the feature flag into the config loader"); err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if gated == nil {
		t.Fatal("review step never parked for an answer")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", driver, h.NMBin, fw, "y", "6", "8")
	cmd.Env = os.Environ()
	screen, err := cmd.Output()
	if err != nil {
		t.Fatalf("drive the TUI: %v\n%s", err, screen)
	}
	rendered := string(screen)
	if strings.Contains(rendered, "zero-sized grid") {
		t.Fatalf("the TUI never rendered (zero-sized grid):\n%s", rendered)
	}
	if !strings.Contains(rendered, branch) {
		t.Fatalf("the TUI never rendered the run:\n%s", rendered)
	}
	// "end yolo" is the action bar's label while yolo is ON, so the keypress
	// really engaged the auto-resolver rather than being dropped.
	if !strings.Contains(rendered, "end yolo") {
		t.Fatalf("yolo mode never engaged in the TUI:\n%s", rendered)
	}
	t.Logf("TUI screen after pressing yolo:\n%s", lastScreenFrame(rendered))

	// Yolo saw the gate and stood aside: the review step is still parked on the
	// question, and no fix round ever started.
	after := h.RunInfo(gated.ID)
	step, ok := findStep(after.Steps, types.StepReview)
	if !ok {
		t.Fatal("review step vanished")
	}
	if step.Status != types.StepStatusAwaitingApproval {
		t.Fatalf("review step status = %s after yolo, want still awaiting_approval", step.Status)
	}
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, "Investigate previous review findings") {
			t.Errorf("yolo handed the open review question to the fixer:\n%s", promptTail(inv.Prompt))
		}
	}

	// The answer still releases it, from the same worktree.
	if out, err := h.RunInDir(fw, "axi", "answer", "--question", "q1", "--answer", conversationAnswerText); err != nil {
		t.Fatalf("axi answer after yolo: %v\n%s", err, out)
	}
	if completed := h.WaitForRun(branch, 120*time.Second); completed.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed", completed.Status)
	}
}

// lastScreenFrame returns the tail of a pty transcript, which is the frame a
// reviewer would have been looking at.
func lastScreenFrame(raw string) string {
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	if len(lines) > 60 {
		lines = lines[len(lines)-60:]
	}
	return strings.Join(lines, "\n")
}

// pushedRepoConfigEnablingTheConversation is the .no-mistakes.yaml a
// contributor ships on their own branch to try to turn the conversation on for
// the review that gates them.
const pushedRepoConfigEnablingTheConversation = `ignore_patterns:
  - 'vendor/**'
allow_repo_commands: true
review:
  conversation: true
`

// TestPushedBranchCannotEnableTheReviewConversation is the trust boundary.
// review.conversation decides whether a review may park the run for a human
// answer, so it is read from the trusted default branch only: a pushed branch
// that asks for the conversation gets the review its maintainer configured.
func TestPushedBranchCannotEnableTheReviewConversation(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t)})

	h.CommitChange("init-untrusted-conversation", "seed.txt", "seed\n", "seed for the trust journey")
	initWorktree := h.AddWorktree("init-untrusted-conversation")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	branch := "feature/untrusted-conversation"
	h.CommitChange(branch, "feature.txt", "flag = true\n", "add the feature flag")
	h.CommitChange(branch, ".no-mistakes.yaml", pushedRepoConfigEnablingTheConversation,
		"contributor: turn the review conversation on from my own branch")
	fw := h.AddWorktree(branch)

	if out, err := h.RunInDir(fw, "axi", "run", "--intent", "wire the feature flag into the config loader"); err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if gated == nil {
		t.Fatal("review step never parked")
	}

	prompt := reviewPrompt(t, h)
	if strings.Contains(prompt, "You have a question channel:") {
		t.Errorf("a pushed branch turned the review conversation on:\n%s", promptTail(prompt))
	}
	convDir := reviewqa.Dir(filepath.Join(h.NMHome, "evidence", gated.ID))
	if _, err := os.Stat(convDir); !os.IsNotExist(err) {
		t.Errorf("pushed branch created the conversation directory %s (stat err=%v)", convDir, err)
	}
	out, err := h.RunInDir(fw, "axi", "answer", "--question", "q1", "--answer", "default off")
	if err == nil {
		t.Errorf("axi answer succeeded for a pushed-branch opt-in, want a refusal:\n%s", out)
	}
	if !strings.Contains(out, "review.conversation") {
		t.Errorf("axi answer refusal does not name the trusted setting:\n%s", out)
	}
	t.Logf("axi answer refusal for a pushed-branch opt-in:\n%s", out)
}
