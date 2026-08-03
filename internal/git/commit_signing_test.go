package git

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// forceSigningWithBrokenSigner makes any commit in dir attempt to sign with a
// signer binary that does not exist, standing in for an interactive signer
// (1Password's op-ssh-sign) that the unattended daemon cannot unlock.
func forceSigningWithBrokenSigner(t *testing.T, dir string) {
	t.Helper()
	run(t, dir, "git", "config", "--local", "commit.gpgsign", "true")
	run(t, dir, "git", "config", "--local", "gpg.format", "ssh")
	run(t, dir, "git", "config", "--local", "gpg.ssh.program", filepath.Join(dir, "no-such-signer"))
	run(t, dir, "git", "config", "--local", "user.signingkey", "ssh-ed25519 AAAAfake")
}

func commitFile(t *testing.T, dir, name string) error {
	t.Helper()
	writeFile(t, filepath.Join(dir, name), "content\n")
	run(t, dir, "git", "add", "-A")
	cmd := exec.Command("git", "commit", "-m", "no-mistakes: apply agent fixes")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("git commit output: %s", out)
	}
	return err
}

func TestDisableCommitSigning_LetsUnattendedCommitsSucceedUnderForcedSigning(t *testing.T) {
	ctx := context.Background()
	dir := initTestRepo(t)
	forceSigningWithBrokenSigner(t, dir)

	if err := commitFile(t, dir, "before.txt"); err == nil {
		t.Fatal("expected commit to fail while signing is forced with an unusable signer")
	}

	if err := DisableCommitSigning(ctx, dir); err != nil {
		t.Fatalf("DisableCommitSigning failed: %v", err)
	}

	if err := commitFile(t, dir, "after.txt"); err != nil {
		t.Fatalf("commit after DisableCommitSigning failed: %v", err)
	}
	if got := run(t, dir, "git", "config", "--get", "commit.gpgsign"); got != "false" {
		t.Errorf("commit.gpgsign = %q, want %q", got, "false")
	}
	if got := run(t, dir, "git", "config", "--get", "tag.gpgsign"); got != "false" {
		t.Errorf("tag.gpgsign = %q, want %q", got, "false")
	}
}

// The daemon carves every run worktree from one shared bare gate repo, so the
// write must be per-worktree: an unscoped --local write lands in the bare's
// shared config and two concurrent runs race on <bare>/config.lock (the same
// hazard CopyLocalUserIdentity documents).
func TestDisableCommitSigning_WritesPerWorktreeScopeOnGateWorktrees(t *testing.T) {
	ctx := context.Background()
	bare := t.TempDir()
	run(t, bare, "git", "init", "--bare", ".")
	seed := initTestRepo(t)
	run(t, seed, "git", "remote", "add", "gate", bare)
	run(t, seed, "git", "push", "gate", "HEAD:refs/heads/main")
	run(t, bare, "git", "config", "extensions.worktreeConfig", "true")

	head := run(t, seed, "git", "rev-parse", "HEAD")
	wtA := filepath.Join(t.TempDir(), "a")
	wtB := filepath.Join(t.TempDir(), "b")
	if err := WorktreeAdd(ctx, bare, wtA, head); err != nil {
		t.Fatalf("WorktreeAdd a: %v", err)
	}
	if err := WorktreeAdd(ctx, bare, wtB, head); err != nil {
		t.Fatalf("WorktreeAdd b: %v", err)
	}

	if err := DisableCommitSigning(ctx, wtA); err != nil {
		t.Fatalf("DisableCommitSigning failed: %v", err)
	}

	if got := run(t, wtA, "git", "config", "--worktree", "--get", "commit.gpgsign"); got != "false" {
		t.Errorf("worktree a commit.gpgsign = %q, want %q", got, "false")
	}
	// Read worktree b's EFFECTIVE value, not its own per-worktree scope: a
	// regression that writes to the bare's shared config is invisible to a
	// --worktree read, which is exactly the leak this asserts against.
	cmd := exec.Command("git", "config", "--get", "commit.gpgsign")
	cmd.Dir = wtB
	if out, err := cmd.Output(); err == nil {
		t.Errorf("worktree b inherited the write in shared scope: %q", out)
	}
}

// Where per-worktree config is unusable the disable must fail loudly instead
// of falling back to --local: on a gate worktree that write lands in the bare's
// shared config, where commit.gpgsign=false becomes permanent gate state that
// outlives the sign_commits setting with nothing that ever unsets it.
func TestDisableCommitSigning_RefusesSharedConfigFallback(t *testing.T) {
	ctx := context.Background()
	bare := t.TempDir()
	run(t, bare, "git", "init", "--bare", ".")
	seed := initTestRepo(t)
	run(t, seed, "git", "remote", "add", "gate", bare)
	run(t, seed, "git", "push", "gate", "HEAD:refs/heads/main")

	// No extensions.worktreeConfig: this is the legacy gate shape where the
	// --worktree write is rejected.
	head := run(t, seed, "git", "rev-parse", "HEAD")
	wtA := filepath.Join(t.TempDir(), "a")
	wtB := filepath.Join(t.TempDir(), "b")
	if err := WorktreeAdd(ctx, bare, wtA, head); err != nil {
		t.Fatalf("WorktreeAdd a: %v", err)
	}
	if err := WorktreeAdd(ctx, bare, wtB, head); err != nil {
		t.Fatalf("WorktreeAdd b: %v", err)
	}

	err := DisableCommitSigning(ctx, wtA)
	if err == nil {
		t.Fatal("expected DisableCommitSigning to fail where per-worktree config is unusable")
	}
	if !strings.Contains(err.Error(), "per-worktree") {
		t.Errorf("error = %q, want it to name the per-worktree requirement", err)
	}
	cmd := exec.Command("git", "config", "--get", "commit.gpgsign")
	cmd.Dir = wtB
	if out, cmdErr := cmd.Output(); cmdErr == nil {
		t.Errorf("shared gate config was written: commit.gpgsign = %q", strings.TrimSpace(string(out)))
	}
}
