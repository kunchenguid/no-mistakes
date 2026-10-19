package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// dirtyBeforeDocument leaves the churn an earlier step can strand in the run
// worktree (issue #762): a modified tracked file and an untracked lockfile.
func dirtyBeforeDocument(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("churned by an earlier step\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertStillDirty(t *testing.T, dir string) {
	t.Helper()
	status := gitStatusPorcelain(t, dir)
	for _, want := range []string{"M base.txt", "?? package-lock.json"} {
		if !strings.Contains(status, want) {
			t.Fatalf("pre-existing dirt %q must stay uncommitted in the worktree, status = %q", want, status)
		}
	}
}

func TestDocumentStep_DoesNotCommitPreexistingDirtyFiles(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	dirtyBeforeDocument(t, dir)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Documented\n"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"document feature"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&DocumentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if outcome.FixSummary != changesAppliedSummary {
		t.Fatalf("FixSummary = %q, want %q", outcome.FixSummary, changesAppliedSummary)
	}
	if got := gitCmd(t, dir, "show", "--name-only", "--pretty=format:", "HEAD"); got != "README.md" {
		t.Fatalf("document commit files = %q, want only README.md", got)
	}
	assertStillDirty(t, dir)
}

func TestDocumentStep_NothingToDocumentMakesNoCommitOverPreexistingDirt(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	dirtyBeforeDocument(t, dir)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"docs already in sync; no lint tooling configured in repo"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&DocumentStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if outcome.FixSummary != NoChangesAppliedSummary {
		t.Fatalf("FixSummary = %q, want %q", outcome.FixSummary, NoChangesAppliedSummary)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %s, want no document commit over %s", got, headSHA)
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("run head = %s, want unchanged %s", sctx.Run.HeadSHA, headSHA)
	}
	assertStillDirty(t, dir)
}

// `git commit` takes the whole index, so churn an earlier step already staged
// must be unstaged, not merely left out of the catch-all add.
func TestDocumentStep_DoesNotCommitPreexistingStagedFiles(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	dirtyBeforeDocument(t, dir)
	gitCmd(t, dir, "add", "package-lock.json")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Documented\n"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"document feature"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := (&DocumentStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gitCmd(t, dir, "show", "--name-only", "--pretty=format:", "HEAD"); got != "README.md" {
		t.Fatalf("document commit files = %q, want only README.md", got)
	}
	assertStillDirty(t, dir)
}

// A file the agent edits is the agent's work even when it was already dirty,
// so the edit is never dropped.
func TestDocumentStep_CommitsAgentEditToAnAlreadyDirtyFile(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	dirtyBeforeDocument(t, dir)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "base.txt"), []byte("documented by the agent\n"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"document base"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := (&DocumentStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gitCmd(t, dir, "show", "--name-only", "--pretty=format:", "HEAD"); got != "base.txt" {
		t.Fatalf("document commit files = %q, want only base.txt", got)
	}
	if status := gitStatusPorcelain(t, dir); status != "?? package-lock.json" {
		t.Fatalf("status = %q, want only the untouched pre-existing lockfile left", status)
	}
}
