package steps

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// fileAtRef reports whether path exists in the tree at ref in the given repo.
func fileAtRef(t *testing.T, dir, ref, path string) bool {
	t.Helper()
	cmd := exec.Command("git", "cat-file", "-e", ref+":"+path)
	cmd.Dir = dir
	return cmd.Run() == nil
}

// Issue #281: after no-mistakes opens a PR, a reviewed commit is pushed to
// origin only (not through the gate). When main moves and the CI monitor
// auto-fixes the merge conflict, it rebases from the gate's stale local state
// and force-pushes - discarding the origin-only commit. The lease was anchored
// to a freshly-read ls-remote SHA, which never refuses.
//
// This test reproduces the data loss at the CI auto-fix push boundary: the
// origin branch carries a commit the worktree never saw, the worktree produces
// a new head that does not contain it, and commitAndPush must REFUSE rather
// than overwrite it.
func TestCIStep_CommitAndPush_RefusesToClobberUnseenUpstreamCommit(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature") // origin feature == H1, what no-mistakes last saw

	// Out-of-band: a reviewed commit is pushed to origin only, via a separate
	// clone, so the gate worktree never sees it.
	other := t.TempDir()
	gitCmd(t, other, "clone", upstream, ".")
	gitCmd(t, other, "config", "user.name", "other")
	gitCmd(t, other, "config", "user.email", "other@test.com")
	gitCmd(t, other, "checkout", "feature")
	os.WriteFile(filepath.Join(other, "approved.txt"), []byte("approved review fix"), 0o644)
	gitCmd(t, other, "add", "-A")
	gitCmd(t, other, "commit", "-m", "approved review fix")
	approvedSHA := gitCmd(t, other, "rev-parse", "HEAD")
	gitCmd(t, other, "push", "origin", "feature") // origin feature == H2 (has approved.txt)

	// The CI auto-fix agent produces a new head in the worktree that does NOT
	// contain the approved commit (simulating a rebase from stale local state).
	os.WriteFile(filepath.Join(dir, "ci-fix.txt"), []byte("ci fix"), 0o644)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.HeadSHA = headSHA // gate's last-recorded head == H1

	step := &CIStep{}
	pushed, err := step.commitAndPush(sctx)

	// The push must be refused: origin has a commit the worktree never saw.
	if err == nil {
		t.Fatalf("expected commitAndPush to refuse the divergent force-push, got pushed=%v err=nil", pushed)
	}
	if pushed {
		t.Fatalf("expected no push when refusing, got pushed=true")
	}

	// The approved commit must still be on origin.
	originSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if originSHA != approvedSHA {
		t.Fatalf("origin feature SHA = %s, want %s (approved commit must be preserved)", originSHA, approvedSHA)
	}
	if !fileAtRef(t, upstream, "refs/heads/feature", "approved.txt") {
		t.Fatalf("approved.txt was discarded from origin - data loss")
	}
}

// Issue #305: no-mistakes rebased onto a stale view of upstream and then the
// push step force-pushed the result over an origin head that had advanced
// out-of-band, dropping the commits that landed upstream in the meantime. The
// push step must refuse to force-push over commits it never incorporated.
func TestPushStep_RefusesToClobberAdvancedUpstreamBranch(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	gitCmd(t, dir, "push", "origin", "feature") // last-seen origin == H1, tracking ref set

	// Out-of-band push advances origin/feature with work the worktree never saw.
	other := t.TempDir()
	gitCmd(t, other, "clone", upstream, ".")
	gitCmd(t, other, "config", "user.name", "other")
	gitCmd(t, other, "config", "user.email", "other@test.com")
	gitCmd(t, other, "checkout", "feature")
	os.WriteFile(filepath.Join(other, "upstream.txt"), []byte("landed upstream"), 0o644)
	gitCmd(t, other, "add", "-A")
	gitCmd(t, other, "commit", "-m", "landed upstream")
	advancedSHA := gitCmd(t, other, "rev-parse", "HEAD")
	gitCmd(t, other, "push", "origin", "feature") // origin == H2 (has upstream.txt)

	// The worktree's validated/rebased head does not contain the upstream commit.
	os.WriteFile(filepath.Join(dir, "validated.txt"), []byte("validated"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "validated change")
	h3 := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, h3, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.HeadSHA = h3

	step := &PushStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatalf("expected push to refuse clobbering advanced upstream branch")
	}

	originSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if originSHA != advancedSHA {
		t.Fatalf("origin feature SHA = %s, want %s (upstream commit must be preserved)", originSHA, advancedSHA)
	}
	if !fileAtRef(t, upstream, "refs/heads/feature", "upstream.txt") {
		t.Fatalf("upstream.txt was discarded from origin - data loss")
	}
}

// review-1 regression: a force-push run must not clobber an out-of-band commit
// on the PR branch. The hazard is the lease fast-path: if the rebase step
// refreshes origin/<branch> on a force push, the push step's lastSeen anchor
// equals the live remote head, so resolveForcePushDecision accepts
// `current == lastSeen` WITHOUT the patch-id content check and overwrites the
// out-of-band commit. The fix is twofold: the rebase step leaves the
// remote-tracking ref stale on a force push (so lastSeen stays the last
// *observed* head), and the content check excludes only history reachable from
// the run base. This exercises rebase + push together, the way the daemon runs
// them, so it covers the fast-path the unit tests cannot.
func TestForcePushRun_RefusesToClobberOutOfBandBranchCommit(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	m0 := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	// Feature branch v1 (M0 + A), pushed to origin. This is the gate's last
	// observed branch head and sets the local origin/feature tracking ref.
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("v1"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature v1")
	h1 := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// Out-of-band: a reviewed commit reaches origin/feature only.
	other := t.TempDir()
	gitCmd(t, other, "clone", upstream, ".")
	gitCmd(t, other, "config", "user.name", "other")
	gitCmd(t, other, "config", "user.email", "other@test.com")
	gitCmd(t, other, "checkout", "feature")
	os.WriteFile(filepath.Join(other, "approved.txt"), []byte("approved out-of-band fix"), 0o644)
	gitCmd(t, other, "add", "-A")
	gitCmd(t, other, "commit", "-m", "approved out-of-band fix")
	approvedSHA := gitCmd(t, other, "rev-parse", "HEAD")
	gitCmd(t, other, "push", "origin", "feature") // origin/feature == H1 + C

	// The user force-pushes a rewrite of feature (drops A, adds A') that does NOT
	// contain the out-of-band commit. This is a force push relative to BaseSHA=H1.
	gitCmd(t, dir, "reset", "--hard", m0)
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("v2 rewritten"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature v2 (rewrite)")
	h1prime := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, h1, h1prime, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	// Rebase step runs first (force push detected). It must NOT refresh the
	// origin/feature tracking ref, so the push step still anchors to H1.
	rebaseOutcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("rebase step: %v", err)
	}
	if rebaseOutcome != nil && rebaseOutcome.NeedsApproval {
		t.Fatalf("unexpected approval from rebase on a clean force push: %s", rebaseOutcome.Findings)
	}

	// Push step must refuse: origin/feature carries a commit the rewrite dropped.
	_, err = (&PushStep{}).Execute(sctx)
	if err == nil {
		t.Fatalf("expected push to refuse clobbering the out-of-band commit on a force-push run")
	}

	originSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if originSHA != approvedSHA {
		t.Fatalf("origin feature SHA = %s, want %s (out-of-band commit must be preserved)", originSHA, approvedSHA)
	}
	if !fileAtRef(t, upstream, "refs/heads/feature", "approved.txt") {
		t.Fatalf("approved.txt was discarded from origin on a force-push run - data loss")
	}
}
