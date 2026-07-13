package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These tests are the regression for incident run 01KXC3SD5NZYMERGDS68Z1C8ER:
// the review step committed a CORRECT fix (reviewed head R = incident 04b5f5d),
// a concurrent process (a sibling worktree sharing the bare repo) then reset the
// worktree HEAD to a divergent commit D that lacked the fix (incident a876550),
// and the pipeline's next commit (document) built on D and shipped it. R was not
// even an ancestor of what shipped.
//
// commitAgentFixes must refuse to commit whenever the worktree HEAD is no longer
// a descendant of the head the pipeline itself recorded, so the reviewed change
// cannot be silently lost - while still allowing a legitimate forward agent
// commit (e.g. git rebase --continue).

// TestCommitAgentFixes_RefusesToCommitOnOutOfBandResetHead reproduces the
// incident shape: a concurrent / divergent-sibling reset. It also proves the
// anchor integrity requirement - sctx.Run.HeadSHA at the commit point is the
// reviewed head and is NOT corrupted by the out-of-band reset.
func TestCommitAgentFixes_RefusesToCommitOnOutOfBandResetHead(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContext(t, &mockAgent{name: "codex"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true

	guard := filepath.Join(dir, "guard.sh")

	// 1) Review-fix applies the CORRECT change; the pipeline commits it (04b5f5d).
	if err := os.WriteFile(guard, []byte("FORCE_INCLUDE marker-inversion\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitAgentFixes(sctx, types.StepReview, "guard linked secondmate homes correctly", "address review findings"); err != nil {
		t.Fatalf("review-fix commit: %v", err)
	}
	reviewedHead := sctx.Run.HeadSHA
	if reviewedHead == headSHA {
		t.Fatal("review-fix did not advance head")
	}

	// 2) Out-of-band clobber: a concurrent sibling worktree resets HEAD to a
	//    DIVERGENT commit built from base that does not contain the fix (a876550).
	gitCmd(t, dir, "checkout", "--detach", baseSHA)
	if err := os.WriteFile(guard, []byte("REMOVE_ONLY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "crew minimal r2")
	clobber := gitCmd(t, dir, "rev-parse", "HEAD")

	// Anchor integrity (captain requirement b): the recorded reviewed head is NOT
	// overwritten by the out-of-band worktree reset - it still names 04b5f5d while
	// the worktree HEAD now names the clobber.
	if sctx.Run.HeadSHA != reviewedHead {
		t.Fatalf("anchor corrupted: recorded head %s != reviewed head %s after clobber", sctx.Run.HeadSHA, reviewedHead)
	}
	if clobber == reviewedHead {
		t.Fatal("test setup: clobber must differ from reviewed head")
	}

	// 3) Document step edits docs and tries to commit. It MUST refuse loudly.
	if err := os.WriteFile(filepath.Join(dir, "docs.md"), []byte("corrected docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := commitAgentFixes(sctx, types.StepDocument, "correct secondmate guard documentation", "update docs")
	if err == nil {
		t.Fatal("expected commitAgentFixes to refuse committing on an out-of-band-reset HEAD, got nil")
	}
	if !strings.Contains(err.Error(), "not a descendant") {
		t.Fatalf("expected a head-divergence error, got: %v", err)
	}

	// Nothing shipped: the worktree HEAD is still the clobber (no doc commit was
	// layered on), and the recorded head is unchanged (reviewed fix preserved).
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != clobber {
		t.Fatalf("guard must not have committed: HEAD moved from %s to %s", clobber, got)
	}
	if sctx.Run.HeadSHA != reviewedHead {
		t.Fatalf("recorded head changed to %s; reviewed head %s must be preserved on refusal", sctx.Run.HeadSHA, reviewedHead)
	}
	t.Logf("guard refused divergent clobber: reviewed fix at %s protected", reviewedHead[:8])
}

// TestCommitAgentFixes_RefusesOnBackwardReset covers the other out-of-band shape
// (captain requirement d): a reset BACKWARD to an ancestor of the reviewed head.
// The recorded head is a descendant of the live HEAD, not an ancestor, so the
// guard must still refuse - a backward reset would also silently drop the fix.
func TestCommitAgentFixes_RefusesOnBackwardReset(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContext(t, &mockAgent{name: "codex"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true

	if err := os.WriteFile(filepath.Join(dir, "guard.sh"), []byte("FORCE_INCLUDE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitAgentFixes(sctx, types.StepReview, "apply fix", "fallback"); err != nil {
		t.Fatalf("review-fix commit: %v", err)
	}
	reviewedHead := sctx.Run.HeadSHA

	// Out-of-band backward reset to base (an ancestor of the reviewed head).
	gitCmd(t, dir, "reset", "--hard", baseSHA)

	if err := os.WriteFile(filepath.Join(dir, "docs.md"), []byte("docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := commitAgentFixes(sctx, types.StepDocument, "docs", "fallback")
	if err == nil {
		t.Fatal("expected refusal on a backward-reset HEAD, got nil")
	}
	if !strings.Contains(err.Error(), "not a descendant") {
		t.Fatalf("expected a head-divergence error, got: %v", err)
	}
	if sctx.Run.HeadSHA != reviewedHead {
		t.Fatalf("recorded head must be preserved on refusal, got %s", sctx.Run.HeadSHA)
	}
}

func TestCommitAgentFixes_RefusesResetDuringCommit(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContext(t, &mockAgent{name: "codex"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true

	gitDir := gitCmd(t, dir, "rev-parse", "--git-dir")
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	hook := filepath.Join(gitDir, "hooks", "post-commit")
	hookBody := "#!/bin/sh\ngit reset --hard " + baseSHA + "\n"
	if err := os.WriteFile(hook, []byte(hookBody), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("reviewed fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := commitAgentFixes(sctx, types.StepDocument, "update docs", "fallback")
	if err == nil {
		t.Fatal("expected refusal when HEAD is reset during commit")
	}
	if !strings.Contains(err.Error(), "not a descendant") {
		t.Fatalf("expected a head-divergence error, got: %v", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != baseSHA {
		t.Fatalf("expected hook to reset HEAD to %s, got %s", baseSHA, got)
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("recorded head changed to %s; expected %s", sctx.Run.HeadSHA, headSHA)
	}
}

// TestCommitAgentFixes_AllowsForwardAgentCommit confirms the guard does not
// false-positive when an agent legitimately advances HEAD forward (e.g. a
// `git rebase --continue` during conflict resolution) before the pipeline
// commits its own fixes: the recorded head stays an ancestor, so committing is
// allowed.
func TestCommitAgentFixes_AllowsForwardAgentCommit(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContext(t, &mockAgent{name: "codex"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true

	// Agent makes its own forward commit (descendant of the recorded head).
	if err := os.WriteFile(filepath.Join(dir, "agent.txt"), []byte("agent commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "agent forward commit")
	forward := gitCmd(t, dir, "rev-parse", "HEAD")

	// Pipeline then commits its own working-tree edits on top - must succeed.
	if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("pipeline fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitAgentFixes(sctx, types.StepReview, "apply fix", "fallback"); err != nil {
		t.Fatalf("forward agent commit should be allowed, got: %v", err)
	}
	if _, err := git.Run(sctx.Ctx, dir, "merge-base", "--is-ancestor", forward, sctx.Run.HeadSHA); err != nil {
		t.Fatalf("expected forward commit %s to be an ancestor of new head %s", forward, sctx.Run.HeadSHA)
	}
}

// TestAssertPipelineHeadContinuity_AnchorIsRecordedReviewedHead directly
// exercises the guard (captain requirement b): it anchors on the recorded
// reviewed head (sctx.Run.HeadSHA), NOT on the mutable worktree, and an
// out-of-band reset leaves that anchor intact so the guard still fires.
func TestAssertPipelineHeadContinuity_AnchorIsRecordedReviewedHead(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	sctx := newTestContext(t, &mockAgent{name: "codex"}, dir, baseSHA, headSHA, config.Commands{})

	// Record a reviewed head, then clobber the worktree out from under it.
	sctx.Run.HeadSHA = headSHA
	gitCmd(t, dir, "checkout", "--detach", baseSHA)
	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("divergent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "divergent")

	// The recorded anchor is untouched by the worktree reset.
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("anchor must be the recorded reviewed head %s, got %s", headSHA, sctx.Run.HeadSHA)
	}
	// The guard, comparing the recorded anchor against the live (clobbered) HEAD,
	// refuses.
	if err := assertPipelineHeadContinuity(sctx, types.StepDocument); err == nil {
		t.Fatal("expected guard to refuse when live HEAD diverged from the recorded head")
	}

	// Restoring the worktree to the recorded head makes the guard pass again,
	// proving it is anchored on that exact recorded SHA.
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	if err := assertPipelineHeadContinuity(sctx, types.StepDocument); err != nil {
		t.Fatalf("guard should pass when HEAD equals the recorded head, got %v", err)
	}
}
