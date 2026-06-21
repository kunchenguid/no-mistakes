package steps

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestCommitAgentFixes_GpgSignEnabledCommitsUnsignedWithWarning proves the
// headless-signing fix (audit §4): when commit.gpgsign=true is configured (as
// many enterprise policies require), an autofix commit must still land —
// routed through `git -c commit.gpgsign=false` — and emit a one-time warning
// that the autofix will be unsigned, rather than failing with
// "gpg: signing failed: Inappropriate ioctl for device" or hanging on pinentry.
func TestCommitAgentFixes_GpgSignEnabledCommitsUnsignedWithWarning(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// Simulate an enterprise policy that mandates signed commits. With this
	// set, a plain `git commit` would attempt to sign and fail headlessly.
	gitCmd(t, dir, "config", "commit.gpgsign", "true")

	// Agent produced uncommitted changes.
	if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs strings.Builder
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Log = func(s string) { logs.WriteString(s + "\n") }

	if err := commitAgentFixes(sctx, types.StepReview, "address review findings", "apply review fixes"); err != nil {
		t.Fatalf("commitAgentFixes with gpgsign=true failed: %v", err)
	}

	// The warning must surface so the unsigned autofix is not a silent surprise.
	if !strings.Contains(logs.String(), "commit.gpgsign") || !strings.Contains(logs.String(), "unsigned") {
		t.Fatalf("expected unsigned-commit warning in logs, got:\n%s", logs.String())
	}

	// The commit must have landed and be unsigned (signature status "N").
	gotMsg := gitCmd(t, dir, "log", "-1", "--pretty=%s")
	wantMsg := "no-mistakes(review): address review findings"
	if gotMsg != wantMsg {
		t.Fatalf("commit message = %q, want %q", gotMsg, wantMsg)
	}
	sigStatus := gitCmd(t, dir, "log", "-1", "--pretty=%G?")
	if sigStatus != "N" {
		t.Fatalf("autofix commit signature status = %q, want \"N\" (no signature); the commit must be unsigned", sigStatus)
	}

	// HeadSHA must advance and be persisted.
	newHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if newHead == headSHA {
		t.Fatal("expected HEAD to advance after the autofix commit")
	}
	if sctx.Run.HeadSHA != newHead {
		t.Fatalf("Run.HeadSHA = %s, want %s", sctx.Run.HeadSHA, newHead)
	}
}

// TestCommitAgentFixes_HungHookFailsFast proves the fail-fast fix (audit §13):
// when a commit would hang (interactive pre-commit hook, SSH passphrase prompt,
// 2FA), the scoped timeout on the autofix commit must fail the step within
// seconds instead of wedging the pipeline until the run's CI timeout.
//
// It routes the commit through a fake `git` binary (the test binary) that
// blocks forever on the commit subcommand, so the timeout cancellation kills
// the direct child cleanly with no orphaned subprocess.
//
// This test is intentionally NOT parallel: it temporarily mutates the
// package-level autofixGitTimeout, and commitAgentFixes (used by other tests
// in this package) reads it, so concurrent run/restore would race.
func TestCommitAgentFixes_HungHookFailsFast(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	// Uncommitted agent changes for commitAgentFixes to commit.
	if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	env := fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":     "git-commit-hang",
		"FAKE_CLI_REAL_GIT": realGit,
	})

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Env = env

	// Shrink the autofix timeout so the test fails fast rather than waiting
	// minutes. Restore the production default afterwards.
	prev := autofixGitTimeout
	autofixGitTimeout = 300 * time.Millisecond
	t.Cleanup(func() { autofixGitTimeout = prev })

	start := time.Now()
	err = commitAgentFixes(sctx, types.StepReview, "address review findings", "apply review fixes")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected commitAgentFixes to fail when the commit hangs, got nil")
	}
	// Must fail within a small bound, far below the hung process's lifetime and
	// far below any realistic run/CI timeout. Allow generous slack for CI.
	if elapsed > 5*time.Second {
		t.Fatalf("commit did not fail fast: elapsed %s (timeout 300ms)", elapsed)
	}
	// And the commit must not have actually landed (HEAD unchanged).
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD advanced to %s despite the hung commit; want unchanged %s", got, headSHA)
	}
}
