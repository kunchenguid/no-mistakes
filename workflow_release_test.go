package main

import (
	"encoding/json"
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
		block := extractJobBlock(t, content, j.name)
		for _, req := range j.required {
			if !strings.Contains(block, req) {
				t.Fatalf("%s job must contain %q so it runs after skipped validation jobs", j.name, req)
			}
		}
	}
}

// Partial-release protection: release-please must create drafts so that a
// release is never marked "latest" until all binaries and checksums are
// uploaded. A separate finalize job gates the promotion on every asset job
// succeeding.
func TestReleasePleaseConfigCreatesDrafts(t *testing.T) {
	data, err := os.ReadFile("release-please-config.json")
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg struct {
		Packages map[string]struct {
			Draft bool `json:"draft"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	pkg, ok := cfg.Packages["."]
	if !ok {
		t.Fatalf("release-please config missing '.' package")
	}
	if !pkg.Draft {
		t.Fatalf("release-please must create releases as drafts; partial releases would otherwise be marked latest before binaries are uploaded")
	}
}

func TestReleasePleaseConfigForcesTagCreation(t *testing.T) {
	data, err := os.ReadFile("release-please-config.json")
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg struct {
		Packages map[string]struct {
			ForceTagCreation bool `json:"force-tag-creation"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	pkg, ok := cfg.Packages["."]
	if !ok {
		t.Fatalf("release-please config missing '.' package")
	}
	if !pkg.ForceTagCreation {
		t.Fatalf("release-please config must force tag creation so an existing GitHub release cannot silently prevent the tag from being recreated")
	}
}

func TestReleaseWorkflowDoesNotOverrideReleaseType(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	block := extractJobBlock(t, content, "release-please")
	if strings.Contains(block, "release-type:") {
		t.Fatalf("release workflow must not override release-type; release-please should read it from release-please-config.json")
	}
	if !strings.Contains(block, "config-file: release-please-config.json") {
		t.Fatalf("release workflow must point release-please at release-please-config.json")
	}
}

func TestReleaseWorkflowPromotesDraftOnlyAfterAssetsComplete(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	block := extractJobBlock(t, content, "finalize")

	required := []string{
		"!cancelled()",
		"needs.release-please.result == 'success'",
		"needs.build-and-upload.result == 'success'",
		"needs.checksums.result == 'success'",
		"needs.release-please.outputs.release_created == 'true'",
		"gh release edit",
		"--draft=false",
		"--latest=true",
	}
	for _, req := range required {
		if !strings.Contains(block, req) {
			t.Fatalf("finalize job must contain %q so a draft is only promoted to latest after every asset job succeeds", req)
		}
	}

	for _, dep := range []string{"release-please", "build-and-upload", "checksums"} {
		if !strings.Contains(block, "- "+dep) {
			t.Fatalf("finalize job must declare %q in needs so its gate sees all upstream results", dep)
		}
	}
}

func TestExtractJobBlockHandlesCRLF(t *testing.T) {
	lf := "jobs:\n  foo:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo foo\n  bar:\n    runs-on: ubuntu-latest\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")

	block := extractJobBlock(t, crlf, "foo")
	if !strings.Contains(block, "echo foo") {
		t.Fatalf("CRLF block missing foo body: %q", block)
	}
	if strings.Contains(block, "bar:") {
		t.Fatalf("CRLF block must stop before next job: %q", block)
	}
}

func extractJobBlock(t *testing.T, content, name string) string {
	t.Helper()
	content = strings.ReplaceAll(content, "\r\n", "\n")
	header := "\n  " + name + ":\n"
	start := strings.Index(content, header)
	if start < 0 {
		t.Fatalf("could not locate %s job in workflow", name)
	}
	rest := content[start+len(header):]
	idx := 0
	for {
		next := strings.Index(rest[idx:], "\n  ")
		if next < 0 {
			return rest
		}
		pos := idx + next + 1
		if pos+2 >= len(rest) {
			return rest
		}
		ch := rest[pos+2]
		if ch != ' ' && ch != '#' && ch != '\n' {
			return rest[:pos]
		}
		idx = pos + 1
	}
}
