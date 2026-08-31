//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestDocsOnlyBranchRunsValidation proves that a new branch whose only
// committed changes are project instruction markdown still goes through the
// ordinary validation and delivery gates. In particular, document analysis
// being a no-op must not be confused with an empty branch diff.
func TestDocsOnlyBranchRunsValidation(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
	branch := "feature/docs-only"
	h.Checkout("main")
	if out, err := h.runGit(context.Background(), h.WorkDir, "checkout", "-b", branch); err != nil {
		t.Fatalf("create docs-only branch: %v\n%s", err, out)
	}
	docsWorktree := h.AddWorktree(branch)
	if out, err := h.RunInDir(docsWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	for name, content := range map[string]string{
		"AGENTS.md": "# Agent guidance\n\nKeep the project instructions current.\n",
		"CLAUDE.md": "@AGENTS.md\n",
	} {
		if err := os.WriteFile(filepath.Join(docsWorktree, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := h.runGit(ctx, docsWorktree, "add", "AGENTS.md", "CLAUDE.md"); err != nil {
		t.Fatalf("stage docs-only change: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, docsWorktree, "commit", "-m", "update agent guidance"); err != nil {
		t.Fatalf("commit docs-only change: %v\n%s", err, out)
	}

	if out, err := h.runGit(ctx, docsWorktree, "push", "no-mistakes", branch); err != nil {
		t.Fatalf("push docs-only branch: %v\n%s", err, out)
	}
	run := h.WaitForRun(branch, 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("docs-only run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	for _, name := range []types.StepName{
		types.StepRebase,
		types.StepReview,
		types.StepTest,
		types.StepDocument,
		types.StepLint,
		types.StepPush,
	} {
		step, ok := findStep(run.Steps, name)
		if !ok {
			t.Fatalf("docs-only run missing %s step", name)
		}
		if step.Status != types.StepStatusCompleted {
			t.Fatalf("docs-only run skipped %s; got status %s", name, step.Status)
		}
	}
	for _, name := range []types.StepName{types.StepPR, types.StepCI} {
		step, ok := findStep(run.Steps, name)
		if !ok {
			t.Fatalf("docs-only run missing %s step", name)
		}
		if step.Status != types.StepStatusSkipped {
			t.Fatalf("file-backed docs-only fixture should skip unavailable %s, got %s", name, step.Status)
		}
	}
}
