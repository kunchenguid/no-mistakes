package steps

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// minimalStepContext builds a StepContext with just enough fields for the
// upstream-resolution helpers, without spinning up a database.
func minimalStepContext(t *testing.T, workDir, upstreamURL string) *pipeline.StepContext {
	t.Helper()
	return &pipeline.StepContext{
		Ctx:     context.Background(),
		WorkDir: workDir,
		Repo:    &db.Repo{UpstreamURL: upstreamURL},
	}
}

// TestResolveUpstreamURL_PreservesCredential is the "pushes keep working" half
// of the redaction fix: the DB stores a redacted URL, but the credential must
// still reach the git push/ls-remote argv. The credential is recovered from the
// worktree's "origin" remote (inherited from the gate's bare repo), so
// resolveUpstreamURL must return the full credentialled URL verbatim.
func TestResolveUpstreamURL_PreservesCredential(t *testing.T) {
	t.Parallel()
	const token = "ghp_secret_DO_NOT_LEAK"
	credURL := "https://x-access-token:" + token + "@github.com/o/r.git"
	// The DB copy is redacted (the form gate.Init now persists).
	redacted := "https://redacted@github.com/o/r.git"

	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", credURL).CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}

	sctx := minimalStepContext(t, dir, redacted)
	got := resolveUpstreamURL(sctx)
	if got != credURL {
		t.Errorf("resolveUpstreamURL = %q, want full credentialled URL %q (credential must reach the push argv)", got, credURL)
	}
	if !strings.Contains(got, token) {
		t.Errorf("resolveUpstreamURL stripped the credential: got %q", got)
	}
	if pushURL := resolvePushURL(sctx); pushURL != credURL {
		t.Errorf("resolvePushURL = %q, want credential-preserving upstream route %q", pushURL, credURL)
	}
}

func TestResolveUpstreamURL_PrefersRefreshedRegistration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", "git@example.com:owner/project.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}
	refreshed := "https://example.com/owner/project.git"
	sctx := minimalStepContext(t, dir, refreshed)
	sctx.Repo.URLsVerified = true
	if got := resolveUpstreamURL(sctx); got != refreshed {
		t.Fatalf("resolveUpstreamURL = %q, want refreshed registration %q", got, refreshed)
	}
}

func TestRunUpstreamFetchUsesRefreshedRegistration(t *testing.T) {
	t.Parallel()

	staleUpstream := t.TempDir()
	gitCmd(t, staleUpstream, "init", "--bare")

	seed := t.TempDir()
	gitCmd(t, seed, "init")
	gitCmd(t, seed, "config", "user.name", "test")
	gitCmd(t, seed, "config", "user.email", "test@test.com")
	gitCmd(t, seed, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "base.txt")
	gitCmd(t, seed, "commit", "-m", "stale base")
	gitCmd(t, seed, "remote", "add", "origin", staleUpstream)
	gitCmd(t, seed, "push", "origin", "main")

	refreshedUpstream := t.TempDir()
	gitCmd(t, refreshedUpstream, "clone", "--bare", staleUpstream, ".")

	updater := t.TempDir()
	gitCmd(t, updater, "clone", refreshedUpstream, ".")
	gitCmd(t, updater, "config", "user.name", "test")
	gitCmd(t, updater, "config", "user.email", "test@test.com")
	gitCmd(t, updater, "checkout", "main")
	if err := os.WriteFile(filepath.Join(updater, "base.txt"), []byte("refreshed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, updater, "add", "base.txt")
	gitCmd(t, updater, "commit", "-m", "refreshed base")
	refreshedTip := gitCmd(t, updater, "rev-parse", "HEAD")
	gitCmd(t, updater, "push", "origin", "main")

	workDir := t.TempDir()
	gitCmd(t, workDir, "clone", staleUpstream, ".")
	sctx := minimalStepContext(t, workDir, refreshedUpstream)
	sctx.Repo.URLsVerified = true

	tip, resolved := resolveRunDefaultBranchTip(context.Background(), sctx, "", "main")
	if !resolved {
		t.Fatal("resolveRunDefaultBranchTip reported unresolved")
	}
	if tip != refreshedTip {
		t.Fatalf("resolveRunDefaultBranchTip = %q, want %q", tip, refreshedTip)
	}
	if got := gitCmd(t, workDir, "rev-parse", "origin/main"); got != refreshedTip {
		t.Fatalf("origin/main = %q, want refreshed tip %q", got, refreshedTip)
	}
	if got := gitCmd(t, workDir, "remote", "get-url", "origin"); got != staleUpstream {
		t.Fatalf("origin URL changed to %q, want %q", got, staleUpstream)
	}
}

// TestResolveUpstreamURL_FallsBackToRecordedURL verifies that when a worktree
// has no resolvable "origin" remote, resolveUpstreamURL falls back to the repo
// record's upstream URL (the path old gates whose DB still carries the full URL
// take).
func TestResolveUpstreamURL_FallsBackToRecordedURL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	// No "origin" remote configured.
	recorded := "https://github.com/o/r.git"
	sctx := minimalStepContext(t, dir, recorded)
	if got := resolveUpstreamURL(sctx); got != recorded {
		t.Errorf("resolveUpstreamURL fallback = %q, want %q", got, recorded)
	}
}

// TestResolveBranchBaseSHA_FetchesFreshBaseTipBeforeMergeBase reproduces
// kunchenguid/no-mistakes#997: a worktree's cached origin/<base> ref can be
// stale relative to the live base branch, so computing merge-base against it
// picks an ancestor further back than the branch's real cut point and pulls
// commits that already landed on the base into the PR's drafted diff.
// resolveBranchBaseSHA must fetch the base branch's current remote tip first
// (the same pattern resolveRunDefaultBranchTip already uses) and compute
// merge-base against that fresh tip, not the worktree's stale cached ref.
func TestResolveBranchBaseSHA_FetchesFreshBaseTipBeforeMergeBase(t *testing.T) {
	t.Parallel()

	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare", "-b", "main")

	seed := t.TempDir()
	gitCmd(t, seed, "clone", upstream, ".")
	gitCmd(t, seed, "config", "user.name", "test")
	gitCmd(t, seed, "config", "user.email", "test@test.com")
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("c0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "base.txt")
	gitCmd(t, seed, "commit", "-m", "C0")
	gitCmd(t, seed, "push", "origin", "HEAD:main")

	// workDir is cloned now, while main is still at C0: its cached
	// origin/main ref becomes the stale ref the bug trusted.
	workDir := t.TempDir()
	gitCmd(t, workDir, "clone", upstream, ".")
	gitCmd(t, workDir, "config", "user.name", "test")
	gitCmd(t, workDir, "config", "user.email", "test@test.com")
	staleTip := gitCmd(t, workDir, "rev-parse", "origin/main")

	// main advances on the remote after workDir's clone, before the feature
	// branch is cut - simulating the base moving since the worktree was set
	// up.
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("c1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "base.txt")
	gitCmd(t, seed, "commit", "-m", "C1")
	freshTip := gitCmd(t, seed, "rev-parse", "HEAD")
	gitCmd(t, seed, "push", "origin", "HEAD:main")
	if freshTip == staleTip {
		t.Fatal("test setup: base did not actually advance")
	}

	// The feature branch is cut from the now-current base tip and pushed.
	gitCmd(t, seed, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(seed, "feature.txt"), []byte("f1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "feature.txt")
	gitCmd(t, seed, "commit", "-m", "F1")
	gitCmd(t, seed, "push", "origin", "feature")

	// workDir fetches only the feature branch (as a run's worktree would),
	// leaving its cached origin/main ref stale at C0.
	gitCmd(t, workDir, "fetch", "origin", "feature")
	gitCmd(t, workDir, "checkout", "feature")
	if got := gitCmd(t, workDir, "rev-parse", "origin/main"); got != staleTip {
		t.Fatalf("test setup: origin/main in workDir = %q, want still-stale %q", got, staleTip)
	}

	sctx := minimalStepContext(t, workDir, upstream)
	got, err := resolveBranchBaseSHA(context.Background(), sctx, "", "main")
	if err != nil {
		t.Fatalf("resolveBranchBaseSHA returned unexpected error: %v", err)
	}

	if got == staleTip {
		t.Fatalf("resolveBranchBaseSHA = %q, resolved against the stale cached base tip instead of fetching first (issue #997)", got)
	}
	if got != freshTip {
		t.Fatalf("resolveBranchBaseSHA = %q, want current remote base tip %q", got, freshTip)
	}
}

// TestResolveBranchBaseSHA_FetchFailureIsRefusedNotStaleFallback covers the
// gap kunchenguid flagged on PR #1147 (issue #997's follow-up): a base-branch
// fetch failure (network/auth/etc) must not silently fall back to whatever
// stale origin/<base> ref is already cached in the worktree, since that
// reintroduces the exact stale-base bug this helper exists to eliminate, one
// layer down. resolveBranchBaseSHA must surface the fetch error instead.
func TestResolveBranchBaseSHA_FetchFailureIsRefusedNotStaleFallback(t *testing.T) {
	t.Parallel()

	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare", "-b", "main")

	seed := t.TempDir()
	gitCmd(t, seed, "clone", upstream, ".")
	gitCmd(t, seed, "config", "user.name", "test")
	gitCmd(t, seed, "config", "user.email", "test@test.com")
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("c0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "base.txt")
	gitCmd(t, seed, "commit", "-m", "C0")
	gitCmd(t, seed, "push", "origin", "HEAD:main")

	workDir := t.TempDir()
	gitCmd(t, workDir, "clone", upstream, ".")
	gitCmd(t, workDir, "config", "user.name", "test")
	gitCmd(t, workDir, "config", "user.email", "test@test.com")
	staleTip := gitCmd(t, workDir, "rev-parse", "origin/main")

	gitCmd(t, seed, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(seed, "feature.txt"), []byte("f1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", "feature.txt")
	gitCmd(t, seed, "commit", "-m", "F1")
	gitCmd(t, seed, "push", "origin", "feature")
	gitCmd(t, workDir, "fetch", "origin", "feature")
	gitCmd(t, workDir, "checkout", "feature")

	// Point the resolved upstream at an unreachable location so the base-branch
	// fetch fails (simulating network/auth failure), while workDir's cached
	// origin/main ref stays at the stale tip.
	unreachable := filepath.Join(t.TempDir(), "does-not-exist")
	sctx := minimalStepContext(t, workDir, unreachable)
	// resolveUpstreamURL prefers a verified refreshed registration over the
	// worktree's own origin remote; force it to prefer the unreachable URL so
	// the base-branch fetch actually fails.
	sctx.Repo.URLsVerified = true

	got, err := resolveBranchBaseSHA(context.Background(), sctx, "", "main")
	if err == nil {
		t.Fatalf("resolveBranchBaseSHA succeeded with base %q, want an error surfacing the fetch failure", got)
	}
	if got != "" {
		t.Fatalf("resolveBranchBaseSHA returned base %q on fetch failure, want empty result", got)
	}
	if gotStale := gitCmd(t, workDir, "rev-parse", "origin/main"); gotStale != staleTip {
		t.Fatalf("origin/main moved to %q, want it to remain the stale %q (no fetch should have succeeded)", gotStale, staleTip)
	}
}

// TestResolvePushURL_ForkWinsOverCredential confirms the fork URL takes
// precedence when set (fork-based contribution flow), since fork URLs carry no
// embedded credentials today.
func TestResolvePushURL_ForkWinsOverCredential(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	forkURL := "https://github.com/e-jung/no-mistakes.git"
	sctx := minimalStepContext(t, dir, "https://redacted@github.com/o/r.git")
	sctx.Repo.ForkURL = forkURL
	if got := resolvePushURL(sctx); got != forkURL {
		t.Errorf("resolvePushURL = %q, want fork URL %q", got, forkURL)
	}
}
