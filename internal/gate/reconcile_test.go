package gate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReconcileStaleBranchArchivesPatchEquivalentHeadBeforeNonForcePush(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work := initReconcileRepo(t)
	base := reconcileGit(t, work, "rev-parse", "HEAD")

	writeReconcileFile(t, work, "feature.txt", "same change\n")
	reconcileGit(t, work, "add", "feature.txt")
	reconcileGit(t, work, "commit", "-m", "private rewrite")
	privateHead := reconcileGit(t, work, "rev-parse", "HEAD")

	reconcileGit(t, work, "reset", "--hard", base)
	writeReconcileFile(t, work, "base.txt", "base advanced\n")
	reconcileGit(t, work, "add", "base.txt")
	reconcileGit(t, work, "commit", "-m", "advance base")
	writeReconcileFile(t, work, "feature.txt", "same change\n")
	reconcileGit(t, work, "add", "feature.txt")
	reconcileGit(t, work, "commit", "-m", "rebased private rewrite")
	liveHead := reconcileGit(t, work, "rev-parse", "HEAD")

	gateDir := filepath.Join(t.TempDir(), "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)
	reconcileGit(t, gateDir, "fetch", work, privateHead+":refs/heads/feature/reconcile")

	result, err := ReconcileStaleBranch(ctx, gateDir, work, "feature/reconcile", liveHead)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reconciled || result.PreviousHead != privateHead || result.ArchivedTag == "" {
		t.Fatalf("reconciliation result = %+v", result)
	}
	if got := reconcileGit(t, gateDir, "rev-parse", result.ArchivedTag+"^{commit}"); got != privateHead {
		t.Fatalf("archive tag points at %s, want %s", got, privateHead)
	}
	if out, err := exec.Command("git", "--git-dir="+gateDir, "rev-parse", "--verify", "refs/heads/feature/reconcile").CombinedOutput(); err == nil {
		t.Fatalf("stale branch ref still exists: %s", out)
	}

	// The live head now enters through an ordinary new-branch push. No force or
	// force-with-lease is used by the supported handoff.
	reconcileGit(t, work, "push", gateDir, liveHead+":refs/heads/feature/reconcile")
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/reconcile"); got != liveHead {
		t.Fatalf("non-force push reached %s, want %s", got, liveHead)
	}
}

func TestReconcileStaleBranchLeavesContainedAncestorForNonForcePush(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work := initReconcileRepo(t)

	writeReconcileFile(t, work, "feature.txt", "private content\n")
	reconcileGit(t, work, "add", "feature.txt")
	reconcileGit(t, work, "commit", "-m", "private head")
	privateHead := reconcileGit(t, work, "rev-parse", "HEAD")

	writeReconcileFile(t, work, "live.txt", "later live work\n")
	reconcileGit(t, work, "add", "live.txt")
	reconcileGit(t, work, "commit", "-m", "live descendant")
	liveHead := reconcileGit(t, work, "rev-parse", "HEAD")

	gateDir := filepath.Join(t.TempDir(), "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)
	reconcileGit(t, gateDir, "fetch", work, privateHead+":refs/heads/feature/reconcile")

	result, err := ReconcileStaleBranch(ctx, gateDir, work, "feature/reconcile", liveHead)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reconciled {
		t.Fatalf("ancestor requires no destructive reconciliation: %+v", result)
	}
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/reconcile"); got != privateHead {
		t.Fatalf("ancestor branch moved during inspection to %s, want %s", got, privateHead)
	}
	if tags := reconcileGit(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
		t.Fatalf("ancestor inspection unexpectedly archived a live branch: %q", tags)
	}

	reconcileGit(t, work, "push", gateDir, liveHead+":refs/heads/feature/reconcile")
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/reconcile"); got != liveHead {
		t.Fatalf("non-force fast-forward reached %s, want %s", got, liveHead)
	}
}

func TestReconcileStaleBranchRefusesAndNamesUniquePrivateCommits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work := initReconcileRepo(t)
	base := reconcileGit(t, work, "rev-parse", "HEAD")

	writeReconcileFile(t, work, "private.txt", "unique trailer trim\n")
	reconcileGit(t, work, "add", "private.txt")
	reconcileGit(t, work, "commit", "-m", "private-only trailer trim")
	firstPrivateHead := reconcileGit(t, work, "rev-parse", "HEAD")
	writeReconcileFile(t, work, "second-private.txt", "another unique change\n")
	reconcileGit(t, work, "add", "second-private.txt")
	reconcileGit(t, work, "commit", "-m", "second private-only change")
	privateHead := reconcileGit(t, work, "rev-parse", "HEAD")

	reconcileGit(t, work, "reset", "--hard", base)
	writeReconcileFile(t, work, "live.txt", "different live work\n")
	reconcileGit(t, work, "add", "live.txt")
	reconcileGit(t, work, "commit", "-m", "live branch work")
	liveHead := reconcileGit(t, work, "rev-parse", "HEAD")

	gateDir := filepath.Join(t.TempDir(), "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)
	reconcileGit(t, gateDir, "fetch", work, privateHead+":refs/heads/feature/reconcile")

	result, err := ReconcileStaleBranch(ctx, gateDir, work, "feature/reconcile", liveHead)
	if err == nil {
		t.Fatal("unique private commit was reconciled instead of refused")
	}
	if result.Reconciled {
		t.Fatalf("unique private commit reported reconciliation: %+v", result)
	}
	for _, want := range []string{firstPrivateHead, "private-only trailer trim", privateHead, "second private-only change"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal did not name every at-risk commit; missing %q in: %v", want, err)
		}
	}
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/reconcile"); got != privateHead {
		t.Fatalf("refusal moved private branch to %s, want %s", got, privateHead)
	}
	if tags := reconcileGit(t, gateDir, "tag", "--list", "no-mistakes-abandoned/*"); tags != "" {
		t.Fatalf("refusal created an archive tag despite retaining the branch: %q", tags)
	}
}

func initReconcileRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	reconcileGit(t, "", "init", dir)
	reconcileGit(t, dir, "config", "user.name", "test")
	reconcileGit(t, dir, "config", "user.email", "test@example.com")
	writeReconcileFile(t, dir, "base.txt", "base\n")
	reconcileGit(t, dir, "add", "base.txt")
	reconcileGit(t, dir, "commit", "-m", "base")
	return dir
}

func writeReconcileFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func reconcileGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
