package main

import (
	"os"
	"strings"
	"testing"
)

func TestReleaseWorkflowUsesScopedConcurrencyGroup(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "group: release-${{ github.ref }}") {
		t.Fatalf("release workflow must scope concurrency by ref")
	}
	if strings.Contains(content, "group: release\n") {
		t.Fatalf("release workflow must not use a global concurrency group")
	}
}

func TestReleaseWorkflowSkipsValidationJobsForReleaseCommits(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}

	content := string(data)
	guard := "if: \"!startsWith(github.event.head_commit.message, 'chore(main): release')\""
	if strings.Count(content, guard) != 2 {
		t.Fatalf("release workflow must skip both validation jobs for release commits")
	}
}

func TestReleaseWorkflowAllowsReleasePleaseAfterSkippedValidation(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}

	content := string(data)
	checks := []string{
		"if: |",
		"!cancelled() &&",
		"(needs.check.result == 'success' || needs.check.result == 'skipped') &&",
		"(needs.test.result == 'success' || needs.test.result == 'skipped')",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Fatalf("release workflow must allow release-please to proceed when validation jobs are skipped: missing %q", check)
		}
	}
}
