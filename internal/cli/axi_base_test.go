package cli

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// baseGit runs a git command in dir and fails the test on error.
func baseGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// setupBaseValidationClone builds an origin repo with a "dev" branch, then a
// clone whose cwd the test switches to. It returns the clone dir.
func setupBaseValidationClone(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	baseGit(t, root, "init", "-b", "main", origin)
	baseGit(t, origin, "commit", "--allow-empty", "-m", "initial")
	baseGit(t, origin, "branch", "dev")

	clone := filepath.Join(root, "clone")
	baseGit(t, root, "clone", origin, clone)
	return clone
}

func TestValidateBaseBranch_LocalBranchPasses(t *testing.T) {
	clone := setupBaseValidationClone(t)
	// Create a local-only branch not present on origin's tracking prefix name.
	baseGit(t, clone, "branch", "local-feature")
	t.Chdir(clone)

	if err := validateBaseBranch(context.Background(), "local-feature"); err != nil {
		t.Fatalf("validateBaseBranch(local branch) = %v, want nil", err)
	}
}

func TestValidateBaseBranch_OriginTrackingRefPasses(t *testing.T) {
	clone := setupBaseValidationClone(t)
	t.Chdir(clone)

	// The clone fetched origin/dev into a remote-tracking ref.
	if ok, _ := git.RefExists(context.Background(), ".", "refs/remotes/origin/dev"); !ok {
		t.Fatal("precondition: expected refs/remotes/origin/dev to exist in clone")
	}
	if err := validateBaseBranch(context.Background(), "dev"); err != nil {
		t.Fatalf("validateBaseBranch(origin tracking ref) = %v, want nil", err)
	}
}

func TestValidateBaseBranch_LsRemoteFallbackPasses(t *testing.T) {
	clone := setupBaseValidationClone(t)
	// A branch created on origin after cloning has no local tracking ref, so
	// only the ls-remote fallback can find it.
	originPath := filepath.Join(filepath.Dir(clone), "origin")
	baseGit(t, originPath, "branch", "hotfix")
	t.Chdir(clone)

	if ok, _ := git.RefExists(context.Background(), ".", "refs/remotes/origin/hotfix"); ok {
		t.Fatal("precondition: refs/remotes/origin/hotfix should not exist before fetch")
	}
	if err := validateBaseBranch(context.Background(), "hotfix"); err != nil {
		t.Fatalf("validateBaseBranch(ls-remote fallback) = %v, want nil", err)
	}
}

func TestValidateBaseBranch_NonexistentFails(t *testing.T) {
	clone := setupBaseValidationClone(t)
	t.Chdir(clone)

	err := validateBaseBranch(context.Background(), "does-not-exist")
	if err == nil {
		t.Fatal("validateBaseBranch(nonexistent) = nil, want error")
	}
}
