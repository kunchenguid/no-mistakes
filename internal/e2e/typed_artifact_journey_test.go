//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const typedArtifactLog = "ready synthetic-service\n" +
	"loaded /synthetic/private/report.txt\n" +
	"endpoint=https://synthetic.invalid/api\n" +
	"token=synthetic-secret-value\n" +
	"patient_id=synthetic-private-id\n" +
	"warning synthetic-warning\n" +
	"shutdown complete\n"

func typedArtifactJourneyScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "typed-artifact-journey.yaml")
	content := `actions:
  - match: "Your previous structured findings were REJECTED"
    text: "corrected typed artifacts"
    structured:
      findings: []
      summary: "typed evidence accepted after correction"
      tested:
        - "drove the disposable product journey"
      testing_summary: "Typed evidence was collected from the disposable journey."
      scenarios:
        - name: "typed artifacts reach the pull request safely"
          result: pass
          live: true
          evidence: "captured pull request body"
          reason: ""
      verdict: go
      artifacts:
        - kind: log
          label: ""
          path: fixtures/run.log
          content: "this inline legacy copy must be discarded"
          url: "https://discarded.invalid/log"
        - kind: text-quote
          label: ""
          content: "First  line\r\n\tSecond line"
        - kind: image
          shows: ui
          label: ""
          path: fixtures/ui.png
        - kind: video
          shows: chart
          label: "Chart animation"
          path: fixtures/chart.mp4
        - kind: image
          shows: terminal
          label: "Agent terminal label"
          path: fixtures/terminal.png
  - match: "You are validating a code change by driving the product itself."
    text: "submitted invalid typed artifacts"
    structured:
      findings: []
      summary: "first typed evidence attempt"
      tested:
        - "drove the disposable product journey"
      testing_summary: "Typed evidence was collected from the disposable journey."
      scenarios:
        - name: "typed artifacts reach the pull request safely"
          result: pass
          live: true
          evidence: "captured pull request body"
          reason: ""
      verdict: go
      artifacts:
        - kind: other
          label: "unknown kind"
          path: fixtures/run.log
        - kind: image
          label: "missing posture"
          path: fixtures/ui.png
        - kind: text-quote
          label: "unsafe structure"
          content: "\x60\x60\x60unsafe fence"
  - match: "Draft a pull request title and summary for the full branch delta."
    text: "drafted pull request"
    structured:
      title: "test: exercise typed evidence"
      body: |
        ## What Changed

        - Exercise typed test artifacts in a disposable repository.
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected"
      risk_scope: source-or-external
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write typed artifact scenario: %v", err)
	}
	return path
}

// TestTypedArtifactsReachTheRealPRBoundarySafely drives the built CLI, daemon,
// fake agent wire protocol, Test correction turn, persisted step result, PR
// renderer, and gh boundary in one disposable local journey.
func TestTypedArtifactsReachTheRealPRBoundarySafely(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: typedArtifactJourneyScenario(t)})
	ctx := context.Background()

	parentURL := "https://github.com/synthetic-owner/synthetic-repo.git"
	forkURL := "https://github.com/synthetic-fork/synthetic-repo.git"
	forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "fork.git")
	if err := os.MkdirAll(forkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := h.runGit(ctx, forkDir, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatalf("init fork: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", forkDir, "main"); err != nil {
		t.Fatalf("seed fork: %v\n%s", err, out)
	}
	configureGitURLRewrite(t, h, parentURL, h.UpstreamDir)
	configureGitURLRewrite(t, h, forkURL, forkDir)
	if out, err := h.runGit(ctx, h.WorkDir, "remote", "set-url", "origin", parentURL); err != nil {
		t.Fatalf("set origin: %v\n%s", err, out)
	}

	ghLog := filepath.Join(filepath.Dir(h.AgentLog), "gh-typed-artifacts.log")
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", ghLog)
	t.Setenv("FAKEAGENT_GH_PARENT", "synthetic-owner/synthetic-repo")
	pushMainRepoConfig(t, h, "allow_repo_commands: true\ntest:\n  evidence:\n    attach_media: false\n")

	if out, err := h.Run("init", "--fork-url", forkURL); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	const branch = "feature/typed-artifacts"
	h.CommitChange(branch, "fixtures/run.log", typedArtifactLog, "add synthetic log evidence")
	h.CommitChange(branch, "fixtures/ui.png", "synthetic ui image\n", "add synthetic UI evidence")
	h.CommitChange(branch, "fixtures/chart.mp4", "synthetic chart video\n", "add synthetic chart evidence")
	h.CommitChange(branch, "fixtures/terminal.png", "synthetic terminal image\n", "add synthetic terminal evidence")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, run.Error)
	}
	body := createdPRBody(t, readGHStubInvocations(t, ghLog))

	want := []string{
		"Evidence: Log 1 (not shown in this PR): 7 lines, " + fmt.Sprint(len(typedArtifactLog)) + " bytes, complete; phases: ready 1, error 0, warning 1, shutdown 1",
		"<summary>Evidence: Quote 1</summary>",
		"```text\nFirst  line\n\tSecond line\n```",
		"Evidence: [Image 1]",
		"fixtures/ui.png",
		"Evidence: [Chart animation]",
		"fixtures/chart.mp4",
		"Evidence: Image 2 (not shown in this PR: terminal capture)",
	}
	for _, value := range want {
		if !strings.Contains(body, value) {
			t.Errorf("PR body missing %q:\n%s", value, body)
		}
	}
	for _, forbidden := range []string{
		"synthetic-service", "/synthetic/private/report.txt", "synthetic.invalid/api",
		"synthetic-secret-value", "synthetic-private-id", "this inline legacy copy",
		"discarded.invalid", "Agent terminal label", "fixtures/terminal.png", "```unsafe fence",
		"unknown kind", "missing posture",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("PR body leaked %q:\n%s", forbidden, body)
		}
	}

	correction := findInvocationContaining(h.AgentInvocations(), "Your previous structured findings were REJECTED")
	for _, issue := range []string{`kind "other" is not one of`, "image missing shows", "PR structure (markdown-fence)"} {
		if !strings.Contains(correction, issue) {
			t.Errorf("correction turn missing %q:\n%s", issue, correction)
		}
	}

	if evidenceDir := os.Getenv("NM_TYPED_ARTIFACT_EVIDENCE"); evidenceDir != "" {
		if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
			t.Fatal(err)
		}
		summary := "Real no-mistakes journey completed. The captured PR body contained a machine-only log summary, an exact normalized quote, UI/chart links, and a terminal-media withholding marker. Unknown kind, missing posture, and unsafe quote structure were sent through the correction turn. Synthetic forbidden log values were absent.\n\n" + testingSection(body)
		if err := os.WriteFile(filepath.Join(evidenceDir, "typed-artifact-live-journey.txt"), []byte(summary), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func testingSection(body string) string {
	start := strings.Index(body, "## Testing")
	if start < 0 {
		return "Testing section was not found.\n"
	}
	section := body[start:]
	if next := strings.Index(section[len("## Testing"):], "\n## "); next >= 0 {
		section = section[:len("## Testing")+next]
	}
	return section
}
