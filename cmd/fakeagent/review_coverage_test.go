package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// reviewPromptFor builds the Context block the pipeline's review prompt
// carries, which is all the coverage derivation reads.
func reviewPromptFor(base, ignore string) string {
	return strings.Join([]string{
		reviewPromptMarker + " with a risk assessment.",
		"",
		"Context:",
		"- branch: feature",
		"- base commit: " + base,
		"- target commit: HEAD",
		"- review scope: incremental",
		"- default branch: main",
		"- ignore patterns: " + ignore,
		"",
		"Task:",
	}, "\n")
}

func setupCoverageRepo(t *testing.T) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	gitCmd := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd("init", "-q", "-b", "main")
	write("README.md", "base\n")
	gitCmd("add", ".")
	gitCmd("commit", "-q", "-m", "base")
	base = gitCmd("rev-parse", "HEAD")
	write("feature.txt", "feature\n")
	write("gen.generated.go", "package gen\n")
	write("vendor/dep/dep.go", "package dep\n")
	write("README.md", "changed\n")
	gitCmd("add", ".")
	gitCmd("commit", "-q", "-m", "change")
	return dir, base
}

func TestMatchFillsReviewedPathsForAReviewTurn(t *testing.T) {
	dir, base := setupCoverageRepo(t)
	action := defaultScenario().MatchInDir(dir, reviewPromptFor(base, "*.generated.go, vendor/**"))

	got, ok := action.Structured["reviewed_paths"].([]string)
	if !ok {
		t.Fatalf("reviewed_paths = %#v, want the derived []string", action.Structured["reviewed_paths"])
	}
	want := []string{"README.md", "feature.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reviewed_paths = %v, want %v (ignored files excluded)", got, want)
	}
	// The scenario's own default action is never mutated in place.
	if _, present := defaultScenario().Actions[0].Structured["reviewed_paths"]; present {
		t.Fatal("defaultScenario action gained reviewed_paths; the fill must copy")
	}
}

func TestMatchLeavesExplicitReviewedPathsAlone(t *testing.T) {
	dir, base := setupCoverageRepo(t)
	s := &Scenario{Actions: []Action{{
		Structured: map[string]any{"findings": []any{}, "reviewed_paths": []string{"feature.txt"}},
	}}}
	action := s.MatchInDir(dir, reviewPromptFor(base, "none"))
	got := action.Structured["reviewed_paths"].([]string)
	if !reflect.DeepEqual(got, []string{"feature.txt"}) {
		t.Fatalf("explicit partial reviewed_paths = %v, want it passed through untouched", got)
	}
}

func TestMatchDoesNotFillReviewedPathsOutsideAReviewTurn(t *testing.T) {
	dir, base := setupCoverageRepo(t)
	prompt := strings.Replace(reviewPromptFor(base, "none"), reviewPromptMarker, "Investigate previous review findings", 1)
	action := defaultScenario().MatchInDir(dir, prompt)
	if _, present := action.Structured["reviewed_paths"]; present {
		t.Fatal("a non-review turn gained reviewed_paths")
	}
	raw := &Scenario{Actions: []Action{{StructuredRaw: `{"summary":123}`}}}
	if got := raw.MatchInDir(dir, reviewPromptFor(base, "none")); got.StructuredRaw != `{"summary":123}` {
		t.Fatalf("raw structured payload changed to %q", got.StructuredRaw)
	}
}

func TestReviewCoverageReportsAnEmptySetAsPresent(t *testing.T) {
	dir, base := setupCoverageRepo(t)
	paths, err := reviewCoverageForPrompt(dir, reviewPromptFor(base, "*.md, *.txt, *.generated.go, vendor/**"))
	if err != nil {
		t.Fatal(err)
	}
	if paths == nil || len(paths) != 0 {
		t.Fatalf("paths = %#v, want an empty non-nil slice", paths)
	}
}
