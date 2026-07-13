package steps

import (
	"context"
	"os/exec"
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
