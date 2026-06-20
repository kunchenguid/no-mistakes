package steps

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
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
// still reach the git push/ls-remote argv. The credential is recovered from
// the worktree's "origin" remote (inherited from the gate's bare repo), so
// resolveUpstreamURL must return the full credentialled URL verbatim.
func TestResolveUpstreamURL_PreservesCredential(t *testing.T) {
	t.Parallel()
	const token = "ghp_secret_DO_NOT_LEAK"
	credURL := "https://x-access-token:" + token + "@github.com/o/r.git"
	redacted := git.RedactURL(credURL)

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
// record's upstream URL (the historical behavior, and the path old gates whose
// DB still carries the full URL take).
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

// TestResolveUpstreamURL_FallsBackForCredentialledRecordedURL covers the
// backward-compat case: a gate created before this fix has the full
// credentialled URL still in the DB, and a worktree without an origin remote
// must still push using that recorded URL.
func TestResolveUpstreamURL_FallsBackForCredentialledRecordedURL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	recorded := "https://x-access-token:ghp_legacy@github.com/o/r.git"
	sctx := minimalStepContext(t, dir, recorded)
	if got := resolveUpstreamURL(sctx); got != recorded {
		t.Errorf("resolveUpstreamURL legacy fallback = %q, want %q", got, recorded)
	}
}

// TestPushStepRedactsCredentialURL runs the push step against a worktree whose
// origin carries an embedded credential, and asserts the credential never
// appears in either the user-visible step log or the step's returned error
// (which carries the git ls-remote failure). The unreachable loopback origin
// makes ls-remote fail fast without real network I/O.
func TestPushStepRedactsCredentialURL(t *testing.T) {
	t.Parallel()
	const token = "ghp_secret_DO_NOT_LEAK"
	credURL := "https://x-access-token:" + token + "@127.0.0.1:1/o/r.git"

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	gitCmd(t, dir, "checkout", "-b", "feature")
	baseSHA := gitCmd(t, dir, "rev-parse", "main")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", credURL)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = credURL // DB record also credentialled (old style)

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &PushStep{}
	_, err := step.Execute(sctx)
	// ls-remote to an unreachable origin is expected to fail; the point of
	// the test is that the failure does not leak the credential.
	if err == nil {
		t.Fatal("expected push step to fail against unreachable credentialled origin")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("push step error leaked credential: %v", err)
	}
	for _, l := range logs {
		if strings.Contains(l, token) {
			t.Errorf("push step log leaked credential: %q", l)
		}
	}
	// Sanity: the redacted "pushing to" line was actually emitted.
	var sawPushLog bool
	for _, l := range logs {
		if strings.Contains(l, "pushing to") {
			sawPushLog = true
			if !strings.Contains(l, "***@127.0.0.1:1") {
				t.Errorf("push log not redacted as expected: %q", l)
			}
		}
	}
	if !sawPushLog {
		t.Errorf("expected a 'pushing to' log line, got: %v", logs)
	}
}
