package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func assertOneCommitWithOnly(t *testing.T, dir, parent, file string) {
	t.Helper()
	if got := gitCmd(t, dir, "rev-parse", "HEAD^"); got != parent {
		t.Fatalf("HEAD^ = %s, want one step commit over %s", got, parent)
	}
	if got := gitCmd(t, dir, "show", "--name-only", "--pretty=format:", "HEAD"); got != file {
		t.Fatalf("step commit files = %q, want only %s", got, file)
	}
}

// The document agent commits for itself while an earlier step's lockfile is
// already staged. `git commit` takes the whole index, so the agent's commit
// carries the lockfile and leaves a clean worktree (issue #762, Greptile P1).
func TestDocumentStep_AgentCommitDoesNotKeepPreexistingStagedFile(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	dirtyBeforeDocument(t, dir)
	gitCmd(t, dir, "add", "package-lock.json")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Documented\n"), 0o644)
			gitCmd(t, dir, "add", "README.md")
			gitCmd(t, dir, "commit", "-q", "-m", "docs: agent's own commit")
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
	assertOneCommitWithOnly(t, dir, headSHA, "README.md")
	if sctx.Run.HeadSHA != gitCmd(t, dir, "rev-parse", "HEAD") {
		t.Fatalf("run head = %s, want the worktree head", sctx.Run.HeadSHA)
	}
	assertStillDirty(t, dir)
}

// `git commit -a` sweeps in a tracked file an earlier step modified.
func TestDocumentStep_AgentCommitAllDoesNotKeepPreexistingModifiedFile(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	dirtyBeforeDocument(t, dir)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Documented\n"), 0o644)
			gitCmd(t, dir, "add", "README.md")
			gitCmd(t, dir, "commit", "-q", "-a", "-m", "docs: agent's own commit")
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"document feature"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := (&DocumentStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	assertOneCommitWithOnly(t, dir, headSHA, "README.md")
	assertStillDirty(t, dir)
}

// An agent commit holding nothing but inherited changes is not document work.
func TestDocumentStep_AgentCommitOfOnlyPreexistingDirtMakesNoCommit(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	dirtyBeforeDocument(t, dir)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			gitCmd(t, dir, "commit", "-q", "-a", "-m", "docs: nothing of mine")
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"docs already in sync"}`)}, nil
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
		t.Fatalf("HEAD = %s, want no commit over %s", got, headSHA)
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("run head = %s, want unchanged %s", sctx.Run.HeadSHA, headSHA)
	}
	assertStillDirty(t, dir)
}

// Lint runs right after Document. Without its own snapshot the same untouched
// lockfile would be committed as "fix lint issues".
func TestLintStep_AgentLintDoesNotCommitPreexistingDirtyFiles(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	dirtyBeforeDocument(t, dir)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "formatted.go"), []byte("package main\n"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"format code"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := (&LintStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	assertOneCommitWithOnly(t, dir, headSHA, "formatted.go")
	assertStillDirty(t, dir)
}

// With commands.lint configured, a fix round commits through executeFixMode,
// the path Review and Test fix rounds share.
func TestLintStep_ConfiguredFixRoundDoesNotCommitPreexistingDirtyFiles(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	dirtyBeforeDocument(t, dir)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "formatted.go"), []byte("package main\n"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"format code"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "true"})
	sctx.Fixing = true

	if _, err := (&LintStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	assertOneCommitWithOnly(t, dir, headSHA, "formatted.go")
	assertStillDirty(t, dir)
}
