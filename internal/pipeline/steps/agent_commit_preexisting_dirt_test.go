package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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

// Test can commit work without updating Run.HeadSHA, then leave more edits in
// the same file. A later pass must preserve that commit even if its own agent
// commits the inherited dirt. Only commits made after the snapshot are ours.
func TestAgentCommitRecovery_PreservesCommitsBeforePass(t *testing.T) {
	t.Parallel()
	for _, step := range []struct {
		name     string
		step     pipeline.Step
		commands config.Commands
		fixing   bool
	}{
		{name: "document", step: &DocumentStep{}},
		{name: "lint", step: &LintStep{}},
		{name: "lint-fix", step: &LintStep{}, commands: config.Commands{Lint: "true"}, fixing: true},
	} {
		step := step
		t.Run(step.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("committed by Test\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "commit", "-q", "-a", "-m", "test: earlier work")
			passHead := gitCmd(t, dir, "rev-parse", "HEAD")

			var currentAction string
			ag := &mockAgent{
				name: "test",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					if currentAction == "commit-dirt-and-edit" {
						if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Documented\n"), 0o644); err != nil {
							t.Fatal(err)
						}
						gitCmd(t, dir, "add", "README.md")
					}
					if currentAction != "no-op" {
						gitCmd(t, dir, "commit", "-q", "-a", "-m", "agent commit")
					}
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"housekeeping"}`)}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, step.commands)
			sctx.Fixing = step.fixing

			for _, action := range []string{"no-op", "commit-dirt", "commit-dirt-and-edit"} {
				currentAction = action
				// Deliberately leave the recorded run head behind the live worktree.
				sctx.Run.HeadSHA = headSHA
				dirtyBeforeDocument(t, dir)
				// Also stage package-lock.json on commit-dirt-and-edit to cover
				// pre-staged untracked files alongside modified tracked files.
				if action == "commit-dirt-and-edit" {
					gitCmd(t, dir, "add", "package-lock.json")
				}

				outcome, err := step.step.Execute(sctx)
				if err != nil {
					t.Fatalf("%s: Execute() error = %v", action, err)
				}
				if action == "commit-dirt-and-edit" {
					assertOneCommitWithOnly(t, dir, passHead, "README.md")
				} else if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != passHead {
					t.Fatalf("%s: HEAD = %s, want earlier commit %s preserved", action, got, passHead)
				}
				wantSummary := NoChangesAppliedSummary
				if action == "commit-dirt-and-edit" {
					wantSummary = changesAppliedSummary
				}
				if outcome.FixSummary != wantSummary {
					t.Fatalf("%s: FixSummary = %q, want %q", action, outcome.FixSummary, wantSummary)
				}
				if got := gitCmd(t, dir, "show", "HEAD:base.txt"); got != "committed by Test" {
					t.Fatalf("%s: committed base.txt = %q, want earlier Test work", action, got)
				}
				if got, err := os.ReadFile(filepath.Join(dir, "base.txt")); err != nil || string(got) != "churned by an earlier step\n" {
					t.Fatalf("%s: worktree base.txt = %q, %v; want inherited edits preserved", action, got, err)
				}
				if sctx.Run.HeadSHA != gitCmd(t, dir, "rev-parse", "HEAD") {
					t.Fatalf("%s: run head = %s, want current worktree head", action, sctx.Run.HeadSHA)
				}
				assertStillDirty(t, dir)
			}
		})
	}
}

// Lint runs right after Document. Without its own snapshot the same untouched
// lockfile would be committed as "fix lint issues", both in the agent-driven
// lint pass and in a configured lint fix round (executeFixMode).
func TestLintStep_DoesNotCommitPreexistingDirtyFiles(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
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
		t.Fatalf("agent lint Execute() error = %v", err)
	}
	assertOneCommitWithOnly(t, dir, headSHA, "formatted.go")
	assertStillDirty(t, dir)

	afterAgentLint := sctx.Run.HeadSHA
	sctx.Config.Commands.Lint = "true"
	sctx.Fixing = true
	sctx.Agent = &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "fixed.go"), []byte("package main\n"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix lint"}`)}, nil
		},
	}
	if _, err := (&LintStep{}).Execute(sctx); err != nil {
		t.Fatalf("configured fix Execute() error = %v", err)
	}
	assertOneCommitWithOnly(t, dir, afterAgentLint, "fixed.go")
	assertStillDirty(t, dir)
}
