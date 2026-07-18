package main

import (
	"os"
	"strings"
	"testing"
)

// TestNoMistakesRequiredWorkflowExemptsReleaseAutomation pins the exemption
// logic so the release pipeline (release-please via GITHUB_TOKEN) and
// dependabot are never silently blocked by the gate.
func TestNoMistakesRequiredWorkflowExemptsReleaseAutomation(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/no-mistakes-required.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	exempt := []string{
		"github-actions[bot]",
		"dependabot[bot]",
		"release-please[bot]",
	}
	for _, login := range exempt {
		needle := "github.event.pull_request.user.login != '" + login + "'"
		if !strings.Contains(content, needle) {
			t.Errorf("workflow must exempt %q via %q", login, needle)
		}
	}
}

// TestNoMistakesRequiredWorkflowChecksSignatureMarker pins the exact signature
// string the check greps for. It must match the literal line produced by
// internal/pipeline/steps/prsummary.go when building the Pipeline section.
func TestNoMistakesRequiredWorkflowChecksSignatureMarker(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/no-mistakes-required.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	marker := "Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)"
	if !strings.Contains(content, marker) {
		t.Fatalf("workflow must grep for the prsummary.go signature marker:\n  %s", marker)
	}

	summary, err := os.ReadFile("internal/pipeline/steps/prsummary.go")
	if err != nil {
		t.Fatalf("read prsummary.go: %v", err)
	}
	if !strings.Contains(string(summary), marker) {
		t.Fatalf("prsummary.go no longer writes the expected marker; update both files in sync")
	}
}

// TestNoMistakesRequiredWorkflowReadsCurrentPRBody pins the live lookup that
// avoids evaluating the stale pull_request event snapshot when a fork workflow
// waits for approval while the no-mistakes PR step updates the body.
func TestNoMistakesRequiredWorkflowReadsCurrentPRBody(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/no-mistakes-required.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "pull-requests: read") {
		t.Errorf("workflow must grant read access for the live PR lookup")
	}
	if !strings.Contains(content, "GH_TOKEN: ${{ github.token }}") {
		t.Errorf("workflow must authenticate the live PR lookup")
	}
	if !strings.Contains(content, `gh api "repos/${GITHUB_REPOSITORY}/pulls/${PR_NUMBER}" --jq '.body // ""'`) {
		t.Errorf("workflow must fetch the current PR body at job execution time")
	}
	if strings.Contains(content, "github.event.pull_request.body") {
		t.Errorf("workflow must not rely on the potentially stale event PR body")
	}
}

// TestNoMistakesRequiredWorkflowTriggersOnRelevantPREvents ensures the check
// re-runs when the PR body is edited so a contributor cannot bypass by opening
// clean then editing the body.
func TestNoMistakesRequiredWorkflowTriggersOnRelevantPREvents(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/no-mistakes-required.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	for _, typ := range []string{"opened", "edited", "synchronize", "reopened"} {
		if !strings.Contains(content, typ) {
			t.Errorf("workflow must trigger on pull_request type %q", typ)
		}
	}
}
