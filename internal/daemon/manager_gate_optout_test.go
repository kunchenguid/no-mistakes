package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// gateOptOutWorktree builds a bare gate repo whose default branch carries the
// given .no-mistakes.yaml (empty string => no file), plus a linked worktree with
// origin/main fetched, and returns (wtDir, trustedSHA).
func gateOptOutWorktree(t *testing.T, repoYAML string) (string, string) {
	t.Helper()
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "init", "--initial-branch=main")
	gitCmd(t, src, "config", "user.email", "test@test.com")
	gitCmd(t, src, "config", "user.name", "Test")
	gitCmd(t, src, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if repoYAML != "" {
		if err := os.WriteFile(filepath.Join(src, ".no-mistakes.yaml"), []byte(repoYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-m", "init")

	bare := filepath.Join(t.TempDir(), "bare.git")
	gitCmd(t, "", "init", "--bare", bare)
	if err := git.AddRemote(ctx, bare, "origin", bare); err != nil {
		t.Fatalf("add origin: %v", err)
	}
	gitCmd(t, src, "remote", "add", "origin", bare)
	gitCmd(t, src, "push", "origin", "HEAD:refs/heads/main")

	wt := filepath.Join(t.TempDir(), "wt")
	headSHA := gitOutput(t, src, "rev-parse", "HEAD")
	if err := git.WorktreeAdd(ctx, bare, wt, headSHA); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if err := git.FetchRemoteBranch(ctx, wt, "origin", "main"); err != nil {
		t.Fatalf("fetch main: %v", err)
	}
	sha, err := git.ResolveRef(ctx, wt, "refs/remotes/origin/main")
	if err != nil {
		t.Fatalf("resolve main: %v", err)
	}
	return wt, sha
}

// TestAssertGateTrustedConfigReadable_FileAbsentIsOK proves the common
// ordinary-repo case: the trusted tree is readable and simply has no
// .no-mistakes.yaml, which is NOT opted out and must NOT abort.
func TestAssertGateTrustedConfigReadable_FileAbsentIsOK(t *testing.T) {
	wt, sha := gateOptOutWorktree(t, "")
	if err := assertGateTrustedConfigReadable(context.Background(), wt, "main", sha); err != nil {
		t.Errorf("file legitimately absent must NOT abort, got: %v", err)
	}
}

// TestAssertGateTrustedConfigReadable_PresentAndParseableIsOK proves a readable,
// parseable trusted config (opted out or not) does not abort.
func TestAssertGateTrustedConfigReadable_PresentAndParseableIsOK(t *testing.T) {
	wt, sha := gateOptOutWorktree(t, "disable_project_settings: true\n")
	if err := assertGateTrustedConfigReadable(context.Background(), wt, "main", sha); err != nil {
		t.Errorf("present parseable trusted config must NOT abort, got: %v", err)
	}
	// And the value is honored trusted-only.
	got := loadTrustedRepoConfig(context.Background(), wt, sha, "run")
	if got == nil || !got.DisableProjectSettings {
		t.Errorf("trusted config must carry disable_project_settings=true, got %+v", got)
	}
}

// TestAssertGateTrustedConfigReadable_FetchFailureAborts is the captain's
// security correction: an empty trustedSHA (fetch/resolve failure) must abort
// LOUD, never silently become false.
func TestAssertGateTrustedConfigReadable_FetchFailureAborts(t *testing.T) {
	wt, _ := gateOptOutWorktree(t, "disable_project_settings: true\n")
	err := assertGateTrustedConfigReadable(context.Background(), wt, "main", "")
	if err == nil {
		t.Fatal("empty trustedSHA (fetch failure) must abort")
	}
	if !strings.Contains(err.Error(), "disable_project_settings") {
		t.Errorf("abort error should name the boundary, got: %v", err)
	}
}

// TestAssertGateTrustedConfigReadable_NoDefaultBranchAborts proves an unknown
// default branch (no trusted copy to read) aborts.
func TestAssertGateTrustedConfigReadable_NoDefaultBranchAborts(t *testing.T) {
	wt, sha := gateOptOutWorktree(t, "")
	if err := assertGateTrustedConfigReadable(context.Background(), wt, "", sha); err == nil {
		t.Fatal("empty default branch must abort")
	}
}

// TestAssertGateTrustedConfigReadable_UnreadableCommitAborts proves an
// unresolvable commit (missing object / partial fetch) aborts rather than being
// mistaken for a legitimately-absent file.
func TestAssertGateTrustedConfigReadable_UnreadableCommitAborts(t *testing.T) {
	wt, _ := gateOptOutWorktree(t, "")
	bogus := "0123456789abcdef0123456789abcdef01234567"
	if err := assertGateTrustedConfigReadable(context.Background(), wt, "main", bogus); err == nil {
		t.Fatal("an unreadable trusted commit must abort")
	}
}

func TestAssertGateTrustedConfigReadable_PresentUnreadableBlobAborts(t *testing.T) {
	wt, _ := gateOptOutWorktree(t, "")
	blobCmd := exec.Command("git", "hash-object", "-w", "--stdin")
	blobCmd.Dir = wt
	blobCmd.Stdin = strings.NewReader("disable_project_settings: true\n")
	blobOutput, err := blobCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git hash-object failed: %v\n%s", err, blobOutput)
	}
	blobSHA := strings.TrimSpace(string(blobOutput))

	cmd := exec.Command("git", "mktree")
	cmd.Dir = wt
	cmd.Stdin = strings.NewReader("100644 blob " + blobSHA + "\t.no-mistakes.yaml\n")
	treeOutput, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git mktree failed: %v\n%s", err, treeOutput)
	}
	treeSHA := strings.TrimSpace(string(treeOutput))
	commitSHA := gitOutput(t, wt,
		"-c", "user.name=Test",
		"-c", "user.email=test@test.com",
		"commit-tree", treeSHA, "-m", "missing blob",
	)
	objectsDir := gitOutput(t, wt, "rev-parse", "--git-path", "objects")
	if !filepath.IsAbs(objectsDir) {
		objectsDir = filepath.Join(wt, objectsDir)
	}
	if err := os.Remove(filepath.Join(objectsDir, blobSHA[:2], blobSHA[2:])); err != nil {
		t.Fatalf("remove trusted config blob: %v", err)
	}

	err = assertGateTrustedConfigReadable(context.Background(), wt, "main", commitSHA)
	if err == nil {
		t.Fatal("a present but unreadable trusted config blob must abort")
	}
	if !strings.Contains(err.Error(), "present but not readable") {
		t.Errorf("abort error should distinguish an unreadable blob, got: %v", err)
	}
}

// TestAssertGateTrustedConfigReadable_UnparseableAborts proves a present but
// malformed trusted .no-mistakes.yaml aborts (we cannot evaluate the boundary).
func TestAssertGateTrustedConfigReadable_UnparseableAborts(t *testing.T) {
	wt, sha := gateOptOutWorktree(t, "disable_project_settings: : : {{not yaml\n")
	err := assertGateTrustedConfigReadable(context.Background(), wt, "main", sha)
	if err == nil {
		t.Fatal("unparseable trusted config must abort")
	}
	if !strings.Contains(err.Error(), "unparseable") {
		t.Errorf("abort error should say unparseable, got: %v", err)
	}
}
