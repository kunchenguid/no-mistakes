package custody

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreserveRecoveryHeadCreatesAnchorAndAcceptsSameCommit(t *testing.T) {
	repo, head := recoveryTestRepo(t)
	ctx := context.Background()

	if err := PreserveRecoveryHead(ctx, repo, "run-1", head); err != nil {
		t.Fatalf("create recovery anchor: %v", err)
	}
	if err := PreserveRecoveryHead(ctx, repo, "run-1", head); err != nil {
		t.Fatalf("accept existing matching recovery anchor: %v", err)
	}
	if got := gitOutput(t, repo, "rev-parse", RecoveryRef("run-1")+"^{commit}"); got != head {
		t.Fatalf("recovery anchor = %s, want %s", got, head)
	}
}

func TestPreserveRecoveryHeadRejectsConflictingAnchorWithoutOverwriting(t *testing.T) {
	repo, head := recoveryTestRepo(t)
	gitRun(t, repo, "commit", "--allow-empty", "-m", "other")
	other := gitOutput(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "update-ref", RecoveryRef("run-1"), other)

	err := PreserveRecoveryHead(context.Background(), repo, "run-1", head)
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflicting anchor error = %v", err)
	}
	if got := gitOutput(t, repo, "rev-parse", RecoveryRef("run-1")+"^{commit}"); got != other {
		t.Fatalf("conflicting anchor overwritten: got %s, want %s", got, other)
	}
}

func TestPreserveRecoveryHeadRejectsNonCommitAnchorWithoutOverwriting(t *testing.T) {
	repo, head := recoveryTestRepo(t)
	blobPath := filepath.Join(repo, "blob.txt")
	if err := os.WriteFile(blobPath, []byte("evidence\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blob := gitOutput(t, repo, "hash-object", "-w", blobPath)
	gitRun(t, repo, "update-ref", RecoveryRef("run-1"), blob)

	if err := PreserveRecoveryHead(context.Background(), repo, "run-1", head); err == nil {
		t.Fatal("non-commit recovery anchor was accepted")
	}
	if got := gitOutput(t, repo, "rev-parse", RecoveryRef("run-1")); got != blob {
		t.Fatalf("non-commit anchor overwritten: got %s, want %s", got, blob)
	}
}

func TestPreserveRecoveryAnchorRejectsDanglingSymbolicRefWithoutCreatingTarget(t *testing.T) {
	repo, head := recoveryTestRepo(t)
	ref := RecoveryLocalRef("run-1")
	target := "refs/no-mistakes/evidence/run-1"
	gitRun(t, repo, "symbolic-ref", ref, target)

	if err := PreserveRecoveryAnchor(context.Background(), repo, ref, head); err == nil {
		t.Fatal("dangling symbolic recovery anchor was accepted")
	}
	if got := gitOutput(t, repo, "symbolic-ref", ref); got != target {
		t.Fatalf("symbolic anchor changed: got %s, want %s", got, target)
	}
	cmd := exec.Command("git", "rev-parse", "--verify", target)
	cmd.Dir = repo
	if err := cmd.Run(); err == nil {
		t.Fatalf("dangling symbolic target %s was created", target)
	}
}

func TestPreserveRecoveryAnchorRejectsMatchingSymbolicRef(t *testing.T) {
	repo, head := recoveryTestRepo(t)
	ref := RecoveryGateRef("run-1")
	target := "refs/heads/main"
	gitRun(t, repo, "branch", "main", head)
	gitRun(t, repo, "symbolic-ref", ref, target)

	if err := PreserveRecoveryAnchor(context.Background(), repo, ref, head); err == nil {
		t.Fatal("matching symbolic recovery anchor was accepted")
	}
	if got := gitOutput(t, repo, "symbolic-ref", ref); got != target {
		t.Fatalf("symbolic anchor changed: got %s, want %s", got, target)
	}
}

func recoveryTestRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init")
	gitRun(t, repo, "config", "core.autocrlf", "false")
	gitRun(t, repo, "config", "user.name", "No Mistakes Test")
	gitRun(t, repo, "config", "user.email", "test@example.com")
	gitRun(t, repo, "commit", "--allow-empty", "-m", "base")
	return repo, gitOutput(t, repo, "rev-parse", "HEAD")
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
