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
	sctx.Repo.URLsVerified = true
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

func TestResolveUpstreamURL_RetainsMatchingOriginIdentity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	origin := "git@example.com:owner/project.git"
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", origin).CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}
	sctx := minimalStepContext(t, dir, "https://example.com/owner/project.git")
	sctx.Repo.URLsVerified = true
	if got := resolveUpstreamURL(sctx); got != origin {
		t.Fatalf("resolveUpstreamURL = %q, want matching origin %q", got, origin)
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
