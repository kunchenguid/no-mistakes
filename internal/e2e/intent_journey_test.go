//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestIntentJourney exercises the full user-intent extraction
// path end-to-end:
//
//   - real `no-mistakes init` + post-receive hook + daemon
//   - a Claude transcript fixture seeded under the daemon's $HOME
//   - real Claude reader walking that transcript and matching by cwd
//   - real summarizer prompt sent to the fake agent
//   - real DB persistence of intent_* columns on runs
//   - real injection of the user-intent prompt section into the review
//     step's agent prompt
//
// Failures here usually mean a wiring break between layers (e.g. the
// executor stops populating StepContext.UserIntent, or the prompt helper
// stops being called) - things that the package-level unit tests cannot
// catch because they each stub out one boundary.
func TestIntentJourney(t *testing.T) {
	scenario := writeIntentScenario(t)
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})

	// Seed a Claude transcript under the harness $HOME *before* the daemon
	// starts. The daemon will inherit HOME=h.HomeDir from t.Setenv (set in
	// NewHarness) and `extractIntent` runs at startRun time.
	intentTargetFile := "intent-target.txt"
	seedClaudeTranscript(t, h.HomeDir, h.WorkDir, intentTargetFile)

	// `nm init` from the working directory: register the gate, install the
	// post-receive hook, start the daemon. The daemon resolves repo
	// WorkingPath = h.WorkDir, which is the cwd recorded in our transcript.
	if out, err := h.RunInDir(h.WorkDir, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	// Make a feature branch that touches the same file the transcript
	// mentions. File-overlap matching needs DiffNameOnly(base..head) and
	// the transcript's mentioned paths to intersect.
	branch := "feature/intent-test"
	h.CommitChange(branch, intentTargetFile, "Bar function added\n", "add Bar to "+intentTargetFile)
	h.PushToGate(branch)

	// Wait for the run to terminate. The pipeline runs the full step
	// sequence; clean fakeagent responses keep it under ~30s.
	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want completed; error = %v", run.Status, run.Error)
	}

	invocations := h.AgentInvocations()

	// 1. DB columns populated.
	intent := readRunIntent(t, h.NMHome, run.ID)
	if intent.summary == nil || *intent.summary == "" {
		t.Logf("agent invocations:\n%s", dumpPrompts(invocations))
		t.Fatalf("runs.intent is empty; expected canned summary to be persisted")
	}
	if !strings.Contains(*intent.summary, "Bar()") {
		t.Errorf("runs.intent = %q, want it to contain 'Bar()'", *intent.summary)
	}
	if intent.source == nil || *intent.source != "claude" {
		t.Errorf("runs.intent_source = %v, want 'claude'", intent.source)
	}
	if intent.score == nil || *intent.score <= 0 {
		t.Errorf("runs.intent_score = %v, want > 0", intent.score)
	}

	// 2. Fake agent received the summarizer prompt.
	if !anyInvocationContains(invocations, "transcript of a developer's recent conversation") {
		t.Errorf("expected fakeagent to receive the summarizer prompt, got prompts:\n%s", dumpPrompts(invocations))
	}

	// 3. The review step prompt carried the user-intent section. This is
	// the assertion that catches "intent is computed but never reaches
	// the steps" - the failure mode this whole journey is here to detect.
	reviewPrompt := findInvocationContaining(invocations, "Review the code changes and return structured findings")
	if reviewPrompt == "" {
		t.Fatalf("no review-step prompt observed; agent invocations:\n%s", dumpPrompts(invocations))
	}
	if !strings.Contains(reviewPrompt, "User intent (inferred from the author's recent agent session") {
		t.Errorf("review prompt missing the intent section; review prompt was:\n%s", truncate(reviewPrompt, 2000))
	}
	if !strings.Contains(reviewPrompt, "Bar()") {
		t.Errorf("review prompt does not contain the canned intent body; review prompt was:\n%s", truncate(reviewPrompt, 2000))
	}

	// 4. Every agent invocation - including the summarizer - must have run
	// with the worktree as its cwd. Backends like opencode spawn a
	// long-lived server and lock its cwd from the first call; if the
	// summarizer is invoked without setting CWD, the server roots itself
	// in the daemon's launch directory and every subsequent step (review,
	// test, etc.) operates from the wrong place even when those steps
	// pass the right CWD. The fakeagent records os.Getwd() per call, so
	// this assertion catches the regression generically across backends.
	wantCWD := canonicalForCompare(t, paths.WithRoot(h.NMHome).WorktreeDir(h.repoID(), run.ID))
	for i, inv := range invocations {
		got := canonicalForCompare(t, inv.CWD)
		if got != wantCWD {
			t.Errorf("invocation %d ran in cwd %q, want worktree %q; prompt prefix:\n%s",
				i, inv.CWD, wantCWD, truncate(inv.Prompt, 200))
		}
	}
}

func canonicalForCompare(t *testing.T, p string) string {
	t.Helper()
	if p == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// runIntentColumns is a small bag for the four nullable intent fields read
// directly from the runs table.
type runIntentColumns struct {
	summary   *string
	source    *string
	sessionID *string
	score     *float64
}

func readRunIntent(t *testing.T, nmHome, runID string) runIntentColumns {
	t.Helper()
	p := paths.WithRoot(nmHome)
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open e2e db: %v", err)
	}
	defer database.Close()
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatalf("get run %s: %v", runID, err)
	}
	if run == nil {
		t.Fatalf("run %s not in db", runID)
	}
	return runIntentColumns{
		summary:   run.Intent,
		source:    run.IntentSource,
		sessionID: run.IntentSessionID,
		score:     run.IntentScore,
	}
}

// seedClaudeTranscript writes a minimal but realistic Claude .jsonl file
// at the path the Claude reader will look for it. The transcript records
// cwd = repoCWD so canonicalPath matching succeeds and references
// touchedFile so file-overlap scoring matches whatever change we push.
func seedClaudeTranscript(t *testing.T, homeDir, repoCWD, touchedFile string) {
	t.Helper()
	encoded := testClaudeProjectDirName(repoCWD)
	dir := filepath.Join(homeDir, ".claude", "projects", encoded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir claude projects: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	lines := []string{
		`{"type":"user","cwd":` + testJSONString(t, repoCWD) + `,"timestamp":"` + now + `","uuid":"u1","sessionId":"e2e-session","message":{"role":"user","content":"please add a Bar() helper to ` + touchedFile + `"}}`,
		`{"type":"assistant","cwd":` + testJSONString(t, repoCWD) + `,"timestamp":"` + now + `","uuid":"u2","sessionId":"e2e-session","message":{"role":"assistant","content":[{"type":"text","text":"on it - editing ` + touchedFile + `"},{"type":"tool_use","name":"Edit","input":{"file_path":` + testJSONString(t, filepath.Join(repoCWD, touchedFile)) + `,"old_string":"x","new_string":"Bar()"}}]}}`,
	}
	path := filepath.Join(dir, "e2e-session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write claude transcript: %v", err)
	}
}

func testClaudeProjectDirName(cwd string) string {
	replacer := strings.NewReplacer("/", "-", `\`, "-", ":", "-")
	return replacer.Replace(cwd)
}

func testJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writeIntentScenario writes a fakeagent scenario YAML that returns:
//   - a deterministic summary when invoked with the intent summarizer prompt
//   - the standard "no findings" response for everything else, so the
//     pipeline sails through to completion without needing approval.
//
// Pattern matching is by substring (first match wins), so the summarizer
// entry must come before the catch-all.
func writeIntentScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "intent_scenario.yaml")
	content := `actions:
  - match: "transcript of a developer's recent conversation with a coding agent"
    text: "summarized"
    structured:
      summary: "user wanted Bar() helper added"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      title: "feat: fakeagent change"
      body: "## Summary\nfakeagent canned PR body"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write intent scenario: %v", err)
	}
	return path
}

func anyInvocationContains(invs []Invocation, needle string) bool {
	for _, i := range invs {
		if strings.Contains(i.Prompt, needle) {
			return true
		}
	}
	return false
}

func findInvocationContaining(invs []Invocation, needle string) string {
	for _, i := range invs {
		if strings.Contains(i.Prompt, needle) {
			return i.Prompt
		}
	}
	return ""
}

func dumpPrompts(invs []Invocation) string {
	var sb strings.Builder
	for i, inv := range invs {
		sb.WriteString("--- invocation ")
		sb.WriteString(itoa(i))
		sb.WriteString(" ---\n")
		sb.WriteString(truncate(inv.Prompt, 400))
		sb.WriteString("\n")
	}
	return sb.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
