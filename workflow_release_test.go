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

// Regression for the v1.8.1 incident: when check/test are skipped for release
// PR merge commits, GitHub Actions implicitly wraps downstream job `if:`
// expressions with success(), which returns false because upstream jobs did
// not succeed. build-and-upload and checksums must include !cancelled() (or
// success()/always()) so the implicit wrap is skipped, and must gate on
// release-please succeeding plus release_created=='true'.
func TestReleaseWorkflowRunsBuildAndChecksumsAfterSkippedValidation(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	jobs := []struct {
		name     string
		required []string
	}{
		{
			name: "build-and-upload",
			required: []string{
				"!cancelled()",
				"needs.release-please.result == 'success'",
				"needs.release-please.outputs.release_created == 'true'",
			},
		},
		{
			name: "checksums",
			required: []string{
				"!cancelled()",
				"needs.release-please.result == 'success'",
				"needs.build-and-upload.result == 'success'",
				"needs.release-please.outputs.release_created == 'true'",
			},
		},
	}

	for _, j := range jobs {
		header := "\n  " + j.name + ":\n"
		start := strings.Index(content, header)
		if start < 0 {
			t.Fatalf("could not locate %s job in workflow", j.name)
		}
		rest := content[start+len(header):]
		nextJob := strings.Index(rest, "\n  ")
		for nextJob >= 0 {
			line := rest[nextJob+1:]
			if len(line) > 2 && line[2] != ' ' && line[2] != '#' && line[2] != '\n' {
				break
			}
			next := strings.Index(rest[nextJob+1:], "\n  ")
			if next < 0 {
				nextJob = -1
				break
			}
			nextJob = nextJob + 1 + next
		}
		block := rest
		if nextJob > 0 {
			block = rest[:nextJob]
		}
		for _, req := range j.required {
			if !strings.Contains(block, req) {
				t.Fatalf("%s job must contain %q so it runs after skipped validation jobs", j.name, req)
			}
		}
	}
}
