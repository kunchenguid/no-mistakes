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

// TestNoMistakesRequiredWorkflowReadsPRBodyViaEnv pins the shell-injection-safe
// pattern: the PR body must be piped through an env var, not interpolated
// directly into the shell script body.
func TestNoMistakesRequiredWorkflowReadsPRBodyViaEnv(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/no-mistakes-required.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "PR_BODY: ${{ github.event.pull_request.body }}") {
		t.Errorf("workflow must expose PR body via the PR_BODY env var")
	}
	if strings.Contains(content, "${{ github.event.pull_request.body }}\n          run:") {
		t.Errorf("workflow must not interpolate PR body directly into run: script (injection risk)")
	}
}

// TestNoMistakesRequiredWorkflowRechecksLiveBody pins the stale-snapshot
// fallback: the no-mistakes pipeline pushes the branch (firing synchronize)
// before its PR step rewrites the PR description, so the event payload's body
// snapshot can be momentarily stale. The check must poll the live PR body via
// the API before declaring a violation, rather than relying on a follow-up
// edited event being delivered.
func TestNoMistakesRequiredWorkflowRechecksLiveBody(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/no-mistakes-required.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	needles := []string{
		`gh api "repos/${REPO}/pulls/${PR_NUMBER}"`,
		"GH_TOKEN: ${{ github.token }}",
		"pull-requests: read",
	}
	for _, needle := range needles {
		if !strings.Contains(content, needle) {
			t.Errorf("workflow must re-check the live PR body before failing; missing %q", needle)
		}
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
