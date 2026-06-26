package steps

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// newForcePushFixture builds a local repo whose "origin" points at a bare
// remote, with main + a feature branch pushed to the remote. It returns the
// local dir, a gitRunner bound to it, the remote URL, and the feature head SHA.
func newForcePushFixture(t *testing.T) (dir string, gitRun gitRunner, remote, featureSHA string) {
	t.Helper()
	remote = t.TempDir()
	gitCmd(t, remote, "init", "--bare")

	dir = t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	gitCmd(t, dir, "remote", "add", "origin", remote)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	featureSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	ctx := context.Background()
	gitRun = func(args ...string) (string, error) { return git.Run(ctx, dir, args...) }
	return dir, gitRun, remote, featureSHA
}

func TestResolveForcePushDecision_NewBranch(t *testing.T) {
	t.Parallel()
	_, gitRun, remote, _ := newForcePushFixture(t)
	d, err := resolveForcePushDecision(gitRun, remote, "refs/heads/does-not-exist", "deadbeef", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !d.newBranch {
		t.Fatalf("expected newBranch decision, got %#v", d)
	}
}

func TestResolveForcePushDecision_UpToDate(t *testing.T) {
	t.Parallel()
	_, gitRun, remote, featureSHA := newForcePushFixture(t)
	d, err := resolveForcePushDecision(gitRun, remote, "refs/heads/feature", featureSHA, featureSHA, "")
	if err != nil {
		t.Fatal(err)
	}
	if !d.upToDate {
		t.Fatalf("expected upToDate decision, got %#v", d)
	}
}

func TestResolveForcePushDecision_RemoteUnchangedSinceLastSeen(t *testing.T) {
	t.Parallel()
	dir, gitRun, remote, featureSHA := newForcePushFixture(t)
	// New local head not yet on the remote (e.g. a rebase result).
	os.WriteFile(filepath.Join(dir, "more.txt"), []byte("more"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "more")
	newHead := gitCmd(t, dir, "rev-parse", "HEAD")

	d, err := resolveForcePushDecision(gitRun, remote, "refs/heads/feature", newHead, featureSHA, "")
	if err != nil {
		t.Fatal(err)
	}
	if d.newBranch || d.upToDate || d.remoteSHA != featureSHA {
		t.Fatalf("expected guarded force-push anchored to %s, got %#v", featureSHA, d)
	}
}

func TestResolveForcePushDecision_RefusesUnincorporatedRemoteCommit(t *testing.T) {
	t.Parallel()
	dir, gitRun, remote, featureSHA := newForcePushFixture(t)

	// Out-of-band: the remote feature advances with a commit we never saw.
	other := t.TempDir()
	gitCmd(t, other, "clone", remote, ".")
	gitCmd(t, other, "config", "user.name", "o")
	gitCmd(t, other, "config", "user.email", "o@test.com")
	gitCmd(t, other, "checkout", "feature")
	os.WriteFile(filepath.Join(other, "out_of_band.txt"), []byte("unseen"), 0o644)
	gitCmd(t, other, "add", "-A")
	gitCmd(t, other, "commit", "-m", "out of band")
	gitCmd(t, other, "push", "origin", "feature")

	// Our local head descends from the OLD feature tip, not the new remote tip.
	os.WriteFile(filepath.Join(dir, "local.txt"), []byte("local"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "local")
	newHead := gitCmd(t, dir, "rev-parse", "HEAD")

	// lastSeen is the OLD tip (stale): the remote moved past it out of band.
	_, err := resolveForcePushDecision(gitRun, remote, "refs/heads/feature", newHead, featureSHA, "")
	if err == nil {
		t.Fatal("expected refusal when the remote carries an unincorporated commit")
	}
	if _, ok := err.(*forcePushWouldDiscardError); !ok {
		t.Fatalf("expected forcePushWouldDiscardError, got %T: %v", err, err)
	}
}

// When the remote moved past lastSeen but its commits are already incorporated
// (by content) into the head being pushed, the push is safe and must be allowed
// even though the lease anchor differs from lastSeen.
func TestResolveForcePushDecision_AllowsWhenRemoteContentIncorporated(t *testing.T) {
	t.Parallel()
	dir, gitRun, remote, featureSHA := newForcePushFixture(t)

	// Out-of-band commit on the remote feature.
	other := t.TempDir()
	gitCmd(t, other, "clone", remote, ".")
	gitCmd(t, other, "config", "user.name", "o")
	gitCmd(t, other, "config", "user.email", "o@test.com")
	gitCmd(t, other, "checkout", "feature")
	os.WriteFile(filepath.Join(other, "shared.txt"), []byte("shared work"), 0o644)
	gitCmd(t, other, "add", "-A")
	gitCmd(t, other, "commit", "-m", "shared work")
	remoteTip := gitCmd(t, other, "rev-parse", "HEAD")
	gitCmd(t, other, "push", "origin", "feature")

	// Our local head actually contains that remote commit (we fetched and built
	// on it), so pushing drops nothing.
	gitCmd(t, dir, "fetch", remote, "feature")
	gitCmd(t, dir, "reset", "--hard", remoteTip)
	os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("extra"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "extra on top")
	newHead := gitCmd(t, dir, "rev-parse", "HEAD")

	// lastSeen is stale (the old tip), forcing the content-incorporation check.
	d, err := resolveForcePushDecision(gitRun, remote, "refs/heads/feature", newHead, featureSHA, "")
	if err != nil {
		t.Fatalf("expected push allowed when remote content is incorporated, got %v", err)
	}
	if d.newBranch || d.upToDate || d.remoteSHA != remoteTip {
		t.Fatalf("expected guarded force-push anchored to %s, got %#v", remoteTip, d)
	}
}

// A force push that rewrites history the run already knew (reachable from
// baseSHA) - e.g. dropping a commit by amend/revert - must be allowed: the
// dropped commit is not data loss, it is the deliberate rewrite.
func TestResolveForcePushDecision_AllowsRewriteOfKnownBaseHistory(t *testing.T) {
	t.Parallel()
	dir, gitRun, remote, featureSHA := newForcePushFixture(t)
	base := gitCmd(t, dir, "rev-parse", "main")

	// Rewrite feature: drop the original commit, add a different one. The remote
	// still holds the original (featureSHA), which the rewrite intentionally drops.
	gitCmd(t, dir, "reset", "--hard", base)
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("rewritten"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "rewritten feature")
	newHead := gitCmd(t, dir, "rev-parse", "HEAD")

	// lastSeen empty to force the content check; baseSHA is the prior branch tip
	// the user is knowingly rewriting.
	d, err := resolveForcePushDecision(gitRun, remote, "refs/heads/feature", newHead, "", featureSHA)
	if err != nil {
		t.Fatalf("expected rewrite of known base history to be allowed, got %v", err)
	}
	if d.newBranch || d.upToDate || d.remoteSHA != featureSHA {
		t.Fatalf("expected guarded force-push anchored to %s, got %#v", featureSHA, d)
	}
}

// Even with baseSHA set, a commit that reached the branch out of band (after the
// run base, so not reachable from baseSHA) must still be refused.
func TestResolveForcePushDecision_RefusesOutOfBandEvenWithBase(t *testing.T) {
	t.Parallel()
	dir, gitRun, remote, featureSHA := newForcePushFixture(t)
	base := gitCmd(t, dir, "rev-parse", "main")

	// Out-of-band commit lands on origin/feature on top of featureSHA.
	other := t.TempDir()
	gitCmd(t, other, "clone", remote, ".")
	gitCmd(t, other, "config", "user.name", "o")
	gitCmd(t, other, "config", "user.email", "o@test.com")
	gitCmd(t, other, "checkout", "feature")
	os.WriteFile(filepath.Join(other, "out_of_band.txt"), []byte("unseen"), 0o644)
	gitCmd(t, other, "add", "-A")
	gitCmd(t, other, "commit", "-m", "out of band")
	gitCmd(t, other, "push", "origin", "feature")

	// User rewrites feature off the base; the rewrite contains neither the
	// original commit nor the out-of-band one.
	gitCmd(t, dir, "reset", "--hard", base)
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("rewritten"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "rewritten feature")
	newHead := gitCmd(t, dir, "rev-parse", "HEAD")

	// baseSHA = the prior tip (covers the original commit); the out-of-band
	// commit is a descendant of it, not an ancestor, so it stays flagged.
	_, err := resolveForcePushDecision(gitRun, remote, "refs/heads/feature", newHead, featureSHA, featureSHA)
	if err == nil {
		t.Fatal("expected refusal: an out-of-band commit is not reachable from baseSHA")
	}
	if _, ok := err.(*forcePushWouldDiscardError); !ok {
		t.Fatalf("expected forcePushWouldDiscardError, got %T: %v", err, err)
	}
}
