package steps

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// autofixGitTimeout caps how long a single autofix git commit or push may run.
// It is intentionally far shorter than the run's CI timeout so that a hung
// pre-commit/pre-push hook, an interactive commit-msg hook, or an SSH
// passphrase prompt fails the step quickly instead of wedging the pipeline
// for hours. The derived context still inherits cancellation from the run
// context, so a cancelled run cancels an in-flight autofix op too.
//
// It is a package-level variable (not a constant) so tests can shrink it to
// assert fail-fast behavior without waiting minutes.
var autofixGitTimeout = 2 * time.Minute

// autofixCommit runs `git commit` for an autofix with two headless-safety
// guarantees:
//
//  1. Signing is disabled via `-c commit.gpgsign=false`. Many enterprises set
//     `commit.gpgsign=true` org-wide; an autofix commit cannot be signed
//     headlessly (gpg fails with "Inappropriate ioctl for device" or pinentry
//     hangs until its timeout), so the commit must land unsigned. This is the
//     correct behavior for machine-generated commits. When signing was
//     effectively enabled the user is warned so the unsigned autofix is not a
//     silent surprise.
//
//  2. The commit runs under a short deadline (autofixGitTimeout) so a hook
//     that blocks on stdin (interactive confirmation, 2FA) fails the step
//     quickly instead of hanging until the run's CI timeout.
func autofixCommit(sctx *pipeline.StepContext, message string) error {
	if git.CommitSigningEnabled(sctx.Ctx, sctx.WorkDir) {
		sctx.Log("warning: commit.gpgsign is enabled; no-mistakes autofix commits will be unsigned")
	}
	ctx, cancel := context.WithTimeout(sctx.Ctx, autofixGitTimeout)
	defer cancel()
	_, err := autofixGitRun(ctx, sctx, "-c", "commit.gpgsign=false", "commit", "-m", message)
	if err != nil {
		return fmt.Errorf("commit agent changes: %w", err)
	}
	return nil
}

// autofixPush runs `git push` for an autofix under a short deadline so a hung
// pre-push hook or SSH passphrase prompt fails fast instead of blocking until
// the run's CI timeout.
func autofixPush(sctx *pipeline.StepContext, remote, ref, expectedSHA string, forceWithLease bool) error {
	ctx, cancel := context.WithTimeout(sctx.Ctx, autofixGitTimeout)
	defer cancel()
	args := []string{"push", remote}
	if forceWithLease {
		if expectedSHA != "" {
			args = append(args, fmt.Sprintf("--force-with-lease=%s:%s", ref, expectedSHA))
		} else {
			args = append(args, "--force-with-lease")
		}
	}
	args = append(args, "HEAD:"+ref)
	_, err := autofixGitRun(ctx, sctx, args...)
	return err
}

// autofixGitRun runs a git command for an autofix commit or push. Unlike
// stepGitRunCtx it always applies git.NonInteractiveEnv (so headless git
// semantics — no editor, no credential prompt, no SSH askpass — hold even when
// sctx.Env is empty in production) while still layering sctx.Env on top so
// tests can inject a fake git binary.
func autofixGitRun(ctx context.Context, sctx *pipeline.StepContext, args ...string) (string, error) {
	cmd := stepCmdCtx(ctx, sctx, "git", args...)
	// stepCmdCtx leaves cmd.Env nil when sctx.Env is empty; in both cases layer
	// NonInteractiveEnv underneath sctx.Env so the headless overrides always
	// apply, with test-provided sctx.Env winning on conflict.
	cmd.Env = mergeEnv(append(git.NonInteractiveEnv(sctx.WorkDir), sctx.Env...))
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr)
	}
	return strings.TrimSpace(string(out)), nil
}
