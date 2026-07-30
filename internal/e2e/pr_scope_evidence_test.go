//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const staleTwoFileEvidence = "Inspected only final files: internal/example/flag.go and cmd/example/main.go."

func writeFinalPRScopeScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "final-pr-scope-scenario.yaml")
	content := `actions:
  - match: "Review the code changes and return structured findings"
    text: "review clean"
    structured:
      findings: []
      summary: "review clean"
      risk_level: low
      risk_rationale: "no source risk found"
  - match: "You are validating a code change by testing it. Examine the repository and run the smallest relevant tests yourself."
    text: "two-file test evidence"
    structured:
      findings: []
      summary: "targeted test passed"
      tested:
        - "` + staleTwoFileEvidence + `"
      testing_summary: "Focused validation passed at the test step target commit."
      artifacts: []
  - match: "Perform the combined documentation and lint housekeeping pass for this change."
    text: "documentation updated"
    edits:
      - path: "docs/flag.md"
        new: "# Flag\n"
      - path: "docs/reference.md"
        new: "# Reference\n"
    structured:
      findings: []
      summary: "update flag documentation"
  - match: "Draft a pull request title and summary for the full branch delta."
    text: "full four-file PR summary"
    structured:
      title: "feat: add example flag"
      body: |
        ## What Changed

        - Add flag behavior in ` + "`internal/example/flag.go`" + ` and CLI wiring in ` + "`cmd/example/main.go`" + `.
        - Add documentation in ` + "`docs/flag.md`" + ` and ` + "`docs/reference.md`" + `.
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write final PR scope scenario: %v", err)
	}
	return path
}

// TestPRFinalScopeExcludesEarlierStepEvidence reproduces the PR 1272 failure
// at the closest supported user-visible boundary: a real gate push executes
// the full pipeline and a GitHub PR creation receives the final body on stdin.
//
// Reproduction record, before source-level cause assignment:
//   - Expected behavior: the final PR describes the actual four-file branch
//     delta; earlier Test evidence is step-scoped, never final PR scope.
//   - Observed pre-fix: the PR body preserved two-file Test evidence as though
//     it described the shipped branch after Document added two files.
//   - Initiating trigger: a legitimate Document stage commit after Test.
//   - Masking condition: no later local mutation, or an evidence claim that
//     happens to match the final diff, leaves no visible contradiction.
//   - Visible symptom: a reviewer sees an "only final files" two-file claim
//     in the PR while its branch and PR prompt cover four files.
//   - Earliest divergence from the proven accurate path: the pre-Document Test
//     target is presented in the later PR body instead of remaining evidence
//     for that completed step.
//   - Relevant history: the merged Firstmate PR #1272 supplied the concrete
//     two-file final-scope wording that motivated this regression shape.
//   - Smallest causal counterfactual: if Document made no downstream files,
//     the same two-file evidence would coincide with the final diff and not
//     misstate its scope.
//   - Disconfirming evidence: this test proves the final pushed head, final
//     diff, and PR drafting prompt all contain the two documentation files, so
//     it is not a dropped mutation, stale push, or branch-sync failure.
func TestPRFinalScopeExcludesEarlierStepEvidence(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writeFinalPRScopeScenario(t)})
	ctx := context.Background()

	parentURL := "https://github.com/example/no-mistakes.git"
	forkURL := "https://github.com/example-fork/no-mistakes.git"
	forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "fork.git")
	if err := os.MkdirAll(forkDir, 0o755); err != nil {
		t.Fatalf("mkdir fork: %v", err)
	}
	if out, err := h.runGit(ctx, forkDir, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatalf("init fork: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", forkDir, "main"); err != nil {
		t.Fatalf("seed fork main: %v\n%s", err, out)
	}
	configureGitURLRewrite(t, h, parentURL, h.UpstreamDir)
	configureGitURLRewrite(t, h, forkURL, forkDir)
	if out, err := h.runGit(ctx, h.WorkDir, "remote", "set-url", "origin", parentURL); err != nil {
		t.Fatalf("set parent origin: %v\n%s", err, out)
	}

	ghLog := filepath.Join(filepath.Dir(h.AgentLog), "gh-final-pr-scope.log")
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", ghLog)
	t.Setenv("FAKEAGENT_GH_PARENT", "example/no-mistakes")

	if out, err := h.Run("init", "--fork-url", forkURL); err != nil {
		t.Fatalf("init with fork URL: %v\n%s", err, out)
	}

	const branch = "feature/final-pr-scope"
	h.CommitChange(branch, "internal/example/flag.go", "package example\n", "add flag behavior")
	preDocumentHead := h.CommitChange(branch, "cmd/example/main.go", "package main\n", "add flag CLI")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, run.Error)
	}
	if run.HeadSHA == preDocumentHead {
		t.Fatalf("Document did not advance the tested head %s", preDocumentHead)
	}

	finalHead, err := h.runGit(ctx, forkDir, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read final fork head: %v\n%s", err, finalHead)
	}
	if got := strings.TrimSpace(string(finalHead)); got != run.HeadSHA {
		t.Fatalf("final fork head = %s, want run head %s", got, run.HeadSHA)
	}
	finalDiff, err := h.runGit(ctx, forkDir, "diff", "--name-only", "main..refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read final branch diff: %v\n%s", err, finalDiff)
	}
	wantFiles := []string{
		"cmd/example/main.go",
		"docs/flag.md",
		"docs/reference.md",
		"internal/example/flag.go",
	}
	if got := strings.Fields(string(finalDiff)); !equalStrings(got, wantFiles) {
		t.Fatalf("final branch files = %q, want %q", got, wantFiles)
	}

	testPrompt := findInvocationContaining(h.AgentInvocations(), "You are validating a code change")
	if !strings.Contains(testPrompt, "target commit: "+preDocumentHead) {
		t.Fatalf("Test evidence was not bound to its pre-Document target %s:\n%s", preDocumentHead, testPrompt)
	}
	prPrompt := findInvocationContaining(h.AgentInvocations(), "Draft a pull request title and summary for the full branch delta.")
	for _, want := range append([]string{"target commit: " + run.HeadSHA}, wantFiles...) {
		if !strings.Contains(prPrompt, want) {
			t.Fatalf("final PR drafting prompt missing %q:\n%s", want, prPrompt)
		}
	}

	body := createdPRBody(t, readGHStubInvocations(t, ghLog))
	if strings.Contains(body, "## Testing") {
		t.Fatalf("final PR body must not present earlier Test evidence as final-scope testing:\n%s", body)
	}
	if !strings.Contains(body, "<summary>✅ **Test** - passed</summary>") {
		t.Fatalf("final PR body must retain the Test step status without its stale evidence:\n%s", body)
	}
	for _, want := range wantFiles {
		if !strings.Contains(body, want) {
			t.Fatalf("final PR body missing final-diff file %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, staleTwoFileEvidence) {
		t.Fatalf("earlier two-file Test evidence leaked into final PR scope after Document changed the diff:\n%s", body)
	}
}

func createdPRBody(t *testing.T, invocations []ghStubInvocation) string {
	t.Helper()
	for _, inv := range invocations {
		if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "create" {
			if inv.Body == "" {
				t.Fatalf("PR create did not receive a body on stdin: %+v", inv)
			}
			return inv.Body
		}
	}
	t.Fatalf("no PR create invocation in %+v", invocations)
	return ""
}

func equalStrings(got, want []string) bool {
	return bytes.Equal([]byte(strings.Join(got, "\n")), []byte(strings.Join(want, "\n")))
}
