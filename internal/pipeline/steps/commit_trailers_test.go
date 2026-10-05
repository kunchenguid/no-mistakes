package steps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var attributionTrailers = []string{
	"Co-Authored-By: no-mistakes {{.Agent}} <noreply@example.com>",
	"Assisted-by: no-mistakes:{{.Agent}}:{{.Model}}",
}

// gitTrailers reads trailers back through git's own parser, so the assertion
// is that git recognises them as trailers, not that the text is present.
func gitTrailers(t *testing.T, dir string) string {
	t.Helper()
	return gitCmd(t, dir, "log", "-1", "--format=%(trailers:only,unfold)")
}

func writeAgentChange(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "agent-change.txt"), []byte("change"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCommitAgentFixes_AppendsTrailersForTheAgentThatRan(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	// The configured chain leads with claude, but the fallback ran codex.
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "claude"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Commit = config.Commit{Trailers: attributionTrailers}
	writeAgentChange(t, dir)

	producer := &agent.Result{Provider: "codex", Model: "gpt-5.5"}
	if _, err := commitAgentFixesWithResult(sctx, types.StepReview, "repair widget", "fallback", producer); err != nil {
		t.Fatal(err)
	}
	if got, want := lastCommitMessage(t, dir), "no-mistakes(review): repair widget"; got != want {
		t.Fatalf("commit subject = %q, want %q", got, want)
	}
	want := "Co-Authored-By: no-mistakes codex <noreply@example.com>\nAssisted-by: no-mistakes:codex:gpt-5.5"
	if got := gitTrailers(t, dir); got != want {
		t.Fatalf("commit trailers = %q, want %q", got, want)
	}
}

func TestCommitAgentFixes_TrailersFallBackToStepAgentName(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "claude"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Commit = config.Commit{Trailers: attributionTrailers[1:]}
	writeAgentChange(t, dir)

	// A single configured agent is the bare adapter, which leaves Provider empty.
	producer := &agent.Result{Model: "claude-opus-5-5"}
	if _, err := commitAgentFixesWithResult(sctx, types.StepLint, "fix lint", "fallback", producer); err != nil {
		t.Fatal(err)
	}
	if got, want := gitTrailers(t, dir), "Assisted-by: no-mistakes:claude:claude-opus-5-5"; got != want {
		t.Fatalf("commit trailers = %q, want %q", got, want)
	}
}

func TestCommitAgentFixes_NoProducerMeansNoTrailers(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "claude"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Commit = config.Commit{Trailers: attributionTrailers}
	writeAgentChange(t, dir)

	if err := commitAgentFixes(sctx, types.StepReview, "repair widget", "fallback"); err != nil {
		t.Fatal(err)
	}
	if got := gitTrailers(t, dir); got != "" {
		t.Fatalf("commit trailers = %q, want none for an unattributed commit", got)
	}
}

func TestCommitAgentFixes_InvalidTrailerDoesNotStageChanges(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "claude"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Commit = config.Commit{Trailers: []string{"not a trailer"}}
	writeAgentChange(t, dir)

	if _, err := commitAgentFixesWithResult(sctx, types.StepReview, "repair widget", "fallback", &agent.Result{}); err == nil {
		t.Fatal("commitAgentFixesWithResult() accepted an invalid commit.trailers entry")
	}
	if got := gitCmd(t, dir, "diff", "--cached", "--name-only"); got != "" {
		t.Fatalf("staged files after trailer error = %q, want none", got)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %q, want unchanged %q", got, headSHA)
	}
}

func TestCIStep_CommitRepairAppendsTrailers(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "claude"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.CI.RevalidateRepairs = true
	sctx.Config.Commit = config.Commit{Trailers: attributionTrailers[1:]}
	if err := os.WriteFile(filepath.Join(dir, "ci-fix.txt"), []byte("fixed"), 0o644); err != nil {
		t.Fatal(err)
	}

	repair, err := (&CIStep{}).commitRepair(sctx, effectivePRBaseBranch(sctx), "repair failing checks", &agent.Result{Provider: "grok", Model: "grok-5"})
	if err != nil {
		t.Fatal(err)
	}
	if !repair.HeadAdvanced {
		t.Fatal("CI repair did not advance HEAD")
	}
	if got, want := gitTrailers(t, dir), "Assisted-by: no-mistakes:grok:grok-5"; got != want {
		t.Fatalf("commit trailers = %q, want %q", got, want)
	}
}
