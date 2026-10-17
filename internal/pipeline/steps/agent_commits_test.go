package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const agentAttribution = "Co-Authored-By: Some Agent <noreply@agent.example>"

func agentCommit(t *testing.T, dir, file, subject string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(subject+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", file)
	gitCmd(t, dir, "commit", "-m", subject+"\n\nagent body\n\n"+agentAttribution)
}

func agentCommitContext(t *testing.T, trailers []string) (*pipeline.StepContext, string, string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "claude"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Commit = config.Commit{Trailers: trailers}
	return sctx, dir, headSHA
}

func fullMessage(t *testing.T, dir, rev string) string {
	t.Helper()
	return gitCmd(t, dir, "log", "-1", "--format=%B", rev)
}

func TestCommitAgentFixes_AgentOwnCommitFollowsFixMessageAndNoTrailers(t *testing.T) {
	t.Parallel()
	sctx, dir, headSHA := agentCommitContext(t, nil)
	agentCommit(t, dir, "agent.txt", "fix(tests): free-form agent subject")
	agentHead := gitCmd(t, dir, "rev-parse", "HEAD")
	agentTree := gitCmd(t, dir, "rev-parse", "HEAD^{tree}")
	agentAuthor := gitCmd(t, dir, "log", "-1", "--format=%an <%ae> %ad", "--date=raw")

	committed, err := commitAgentFixesWithResult(sctx, types.StepCI, "repair widget", "fallback", &agent.Result{Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		t.Fatal("committed = true, want false: the pipeline made no commit of its own")
	}
	if got, want := fullMessage(t, dir, "HEAD"), "no-mistakes(ci): repair widget"; got != want {
		t.Fatalf("commit message = %q, want %q", got, want)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD~1"); got != headSHA {
		t.Fatalf("parent = %s, want the recorded head %s", got, headSHA)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD^{tree}"); got != agentTree {
		t.Fatalf("tree changed: %s, want %s", got, agentTree)
	}
	if got := gitCmd(t, dir, "log", "-1", "--format=%an <%ae> %ad", "--date=raw"); got != agentAuthor {
		t.Fatalf("author = %q, want %q", got, agentAuthor)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got == agentHead || sctx.Run.HeadSHA != got {
		t.Fatalf("run head = %s, HEAD = %s, want the rewritten head recorded", sctx.Run.HeadSHA, got)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("worktree not clean: %q", status)
	}
}

func TestCommitAgentFixes_AgentOwnCommitGetsConfiguredTrailersOnly(t *testing.T) {
	t.Parallel()
	sctx, dir, _ := agentCommitContext(t, attributionTrailers[1:])
	agentCommit(t, dir, "agent.txt", "fix: agent subject")

	if _, err := commitAgentFixesWithResult(sctx, types.StepLint, "tidy", "fallback", &agent.Result{Provider: "codex", Model: "gpt-5.5"}); err != nil {
		t.Fatal(err)
	}
	want := "no-mistakes(lint): tidy\n\nAssisted-by: no-mistakes:codex:gpt-5.5"
	if got := fullMessage(t, dir, "HEAD"); got != want {
		t.Fatalf("commit message = %q, want %q", got, want)
	}
	if got := gitTrailers(t, dir); got != "Assisted-by: no-mistakes:codex:gpt-5.5" {
		t.Fatalf("trailers = %q, want only the configured one", got)
	}
}

func TestCommitAgentFixes_EveryAgentCommitInAChainIsRewrittenInOrder(t *testing.T) {
	t.Parallel()
	sctx, dir, headSHA := agentCommitContext(t, nil)
	agentCommit(t, dir, "one.txt", "first")
	agentCommit(t, dir, "two.txt", "second")
	wantTree := gitCmd(t, dir, "rev-parse", "HEAD^{tree}")
	wantFirstTree := gitCmd(t, dir, "rev-parse", "HEAD~1^{tree}")

	if _, err := commitAgentFixesWithResult(sctx, types.StepReview, "repair", "fallback", nil); err != nil {
		t.Fatal(err)
	}
	log := gitCmd(t, dir, "log", "--reverse", "--format=%s", headSHA+"..HEAD")
	if want := "no-mistakes(review): repair\nno-mistakes(review): repair"; log != want {
		t.Fatalf("subjects = %q, want %q", log, want)
	}
	if strings.Contains(gitCmd(t, dir, "log", "--format=%B", headSHA+"..HEAD"), agentAttribution) {
		t.Fatal("agent attribution survived")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD^{tree}"); got != wantTree {
		t.Fatalf("tree changed: %s, want %s", got, wantTree)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD~1^{tree}"); got != wantFirstTree {
		t.Fatalf("first commit tree changed: %s, want %s", got, wantFirstTree)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD~2"); got != headSHA {
		t.Fatalf("chain root = %s, want %s", got, headSHA)
	}
}

func TestCommitAgentFixes_AgentCommitPlusLeftoverEditsBothFollowSettings(t *testing.T) {
	t.Parallel()
	sctx, dir, headSHA := agentCommitContext(t, nil)
	agentCommit(t, dir, "committed.txt", "agent subject")
	writeAgentChange(t, dir)

	committed, err := commitAgentFixesWithResult(sctx, types.StepTest, "repair", "fallback", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("committed = false, want true for the leftover edits")
	}
	if got, want := gitCmd(t, dir, "log", "--format=%B", headSHA+"..HEAD"), "no-mistakes(test): repair\n\nno-mistakes(test): repair"; got != want {
		t.Fatalf("messages = %q, want %q", got, want)
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("worktree not clean: %q", status)
	}
}

func TestCommitAgentFixes_ConformingAgentCommitIsLeftAlone(t *testing.T) {
	t.Parallel()
	sctx, dir, _ := agentCommitContext(t, nil)
	if err := os.WriteFile(filepath.Join(dir, "agent.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "agent.txt")
	gitCmd(t, dir, "commit", "-m", "no-mistakes(review): repair")
	before := gitCmd(t, dir, "rev-parse", "HEAD")

	if _, err := commitAgentFixesWithResult(sctx, types.StepReview, "repair", "fallback", nil); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != before {
		t.Fatalf("HEAD = %s, want unchanged %s", got, before)
	}
	if sctx.Run.HeadSHA != before {
		t.Fatalf("run head = %s, want %s", sctx.Run.HeadSHA, before)
	}
}

func TestCommitAgentFixes_MergeCommitFromAgentIsRefusedUntouched(t *testing.T) {
	t.Parallel()
	sctx, dir, headSHA := agentCommitContext(t, nil)
	agentCommit(t, dir, "left.txt", "left")
	left := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	agentCommit(t, dir, "right.txt", "right")
	gitCmd(t, dir, "merge", "--no-ff", "-m", "merge", left)
	merged := gitCmd(t, dir, "rev-parse", "HEAD")

	_, err := commitAgentFixesWithResult(sctx, types.StepReview, "repair", "fallback", nil)
	if err == nil || !strings.Contains(err.Error(), "not a linear chain") {
		t.Fatalf("err = %v, want a refusal of the non-linear chain", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != merged {
		t.Fatalf("HEAD = %s, want the agent's merge %s kept", got, merged)
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("run head = %s, want unchanged %s", sctx.Run.HeadSHA, headSHA)
	}
}

func TestCommitAgentFixes_InvalidTemplateKeepsAgentCommit(t *testing.T) {
	t.Parallel()
	sctx, dir, _ := agentCommitContext(t, nil)
	sctx.Config.Commit.FixMessage = "{{.Nope}}"
	agentCommit(t, dir, "agent.txt", "agent subject")
	before := gitCmd(t, dir, "rev-parse", "HEAD")

	if _, err := commitAgentFixesWithResult(sctx, types.StepReview, "repair", "fallback", nil); err == nil {
		t.Fatal("expected a template error")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != before {
		t.Fatalf("HEAD = %s, want the agent's commit %s kept", got, before)
	}
}

func TestCommitAgentFixes_CommitsAtOrBelowTheRecordedHeadAreNeverRewritten(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	// The author's own commit is part of the head the run recorded.
	agentCommit(t, dir, "author.txt", "author subject")
	authorHead := gitCmd(t, dir, "rev-parse", "HEAD")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "claude"}, dir, baseSHA, authorHead, config.Commands{})
	agentCommit(t, dir, "agent.txt", "agent subject")

	if _, err := commitAgentFixesWithResult(sctx, types.StepReview, "repair", "fallback", nil); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD~1"); got != authorHead {
		t.Fatalf("author commit rewritten: parent = %s, want %s", got, authorHead)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(review): repair" {
		t.Fatalf("agent commit subject = %q", got)
	}
}

func TestCommitAgentFixes_NoAgentCommitLeavesTheHeadAlone(t *testing.T) {
	t.Parallel()
	sctx, dir, headSHA := agentCommitContext(t, nil)
	if _, err := commitAgentFixesWithResult(sctx, types.StepReview, "repair", "fallback", nil); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %s, want unchanged %s", got, headSHA)
	}
}

func TestExecuteFixMode_AgentOwnCommitFollowsCommitSettings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	ag := &mockAgent{
		name: "claude",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			agentCommit(t, dir, "agent.txt", "fix(tests): free-form agent subject")
			return &agent.Result{Output: json.RawMessage(`{"summary":"repair widget"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true

	if _, err := executeFixMode(sctx, types.StepLint, fixExecutionOptions{Prompt: "fix it"}); err != nil {
		t.Fatal(err)
	}
	if got, want := fullMessage(t, dir, "HEAD"), "no-mistakes(lint): repair widget"; got != want {
		t.Fatalf("commit message = %q, want %q", got, want)
	}
	if sctx.Run.HeadSHA != gitCmd(t, dir, "rev-parse", "HEAD") {
		t.Fatalf("run head = %s, want the rewritten head", sctx.Run.HeadSHA)
	}
}

func TestCIStep_CommitRepair_AgentOwnCommitFollowsCommitSettings(t *testing.T) {
	t.Parallel()
	sctx, dir, headSHA := agentCommitContext(t, nil)
	sctx.Config.CI.RevalidateRepairs = true
	agentCommit(t, dir, "agent.txt", "fix(tests): drop apostrophes from the heredocs")

	if _, err := (&CIStep{}).commitRepair(sctx, "repair CI", &agent.Result{Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	if got, want := fullMessage(t, dir, "HEAD"), "no-mistakes(ci): repair CI"; got != want {
		t.Fatalf("commit message = %q, want %q", got, want)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD~1"); got != headSHA {
		t.Fatalf("parent = %s, want %s", got, headSHA)
	}
}

func TestCIStep_CommitRepair_RetainedAgentCommitFromAFailedTurnFollowsCommitSettings(t *testing.T) {
	t.Parallel()
	sctx, dir, headSHA := agentCommitContext(t, attributionTrailers[1:])
	sctx.Config.CI.RevalidateRepairs = true
	// An earlier turn committed and then failed, so nothing was recorded; the
	// retry has no producer and changes nothing itself.
	agentCommit(t, dir, "agent.txt", "fix(tests): left behind by a failed turn")

	if _, err := (&CIStep{}).commitRepair(sctx, "", nil); err != nil {
		t.Fatal(err)
	}
	if got, want := fullMessage(t, dir, "HEAD"), "no-mistakes(ci): repair failing checks"; got != want {
		t.Fatalf("commit message = %q, want %q", got, want)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD~1"); got != headSHA {
		t.Fatalf("parent = %s, want %s", got, headSHA)
	}
}

func agentMergeCommit(t *testing.T, dir, recorded string) {
	t.Helper()
	agentCommit(t, dir, "left.txt", "left")
	left := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "--detach", recorded)
	agentCommit(t, dir, "right.txt", "right")
	gitCmd(t, dir, "merge", "--no-ff", "-m", "merge", left)
}

func TestCIStep_CommitRepair_MergeFromAnOrdinaryRepairIsRefused(t *testing.T) {
	t.Parallel()
	sctx, dir, _ := agentCommitContext(t, nil)
	sctx.Config.CI.RevalidateRepairs = true
	agentMergeCommit(t, dir, sctx.Run.HeadSHA)
	merged := gitCmd(t, dir, "rev-parse", "HEAD")

	_, err := (&CIStep{}).commitRepair(sctx, "repair", nil)
	if err == nil || !strings.Contains(err.Error(), "not a linear chain") {
		t.Fatalf("err = %v, want a refusal of the non-linear chain", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != merged {
		t.Fatalf("HEAD = %s, want the agent's merge %s kept", got, merged)
	}
}

func TestCIStep_CommitRepair_RebasedHistoryKeepsItsCommitsAndRevalidates(t *testing.T) {
	t.Parallel()
	sctx, dir, _ := agentCommitContext(t, nil)
	sctx.Config.CI.RevalidateRepairs = true
	// A rebase repair rewrites history: the new head does not descend from the
	// recorded one, so its commits are not the agent extending the head.
	gitCmd(t, dir, "checkout", "--detach", sctx.Run.BaseSHA)
	agentCommit(t, dir, "rebased.txt", "rebased subject")
	rebased := gitCmd(t, dir, "rev-parse", "HEAD")

	repair, err := (&CIStep{}).commitRepair(sctx, "repair", nil)
	if err != nil {
		t.Fatalf("a rebasing repair must not be refused: %v", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != rebased {
		t.Fatalf("HEAD = %s, want the rebased history %s untouched", got, rebased)
	}
	if !repair.HeadAdvanced || !repair.Revalidate {
		t.Fatalf("repair = %+v, want the head recorded for revalidation", repair)
	}
}

func TestCIStep_CommitRepair_AlreadyPublishedAgentCommitIsNotRewritten(t *testing.T) {
	t.Parallel()
	sctx, dir, _ := agentCommitContext(t, nil)
	sctx.Config.CI.RevalidateRepairs = true
	// The earlier attempt reached the remote but the run's head was never
	// recorded, so the published commit still sits above the recorded head.
	agentCommit(t, dir, "published.txt", "fix(tests): already on the remote")
	published := gitCmd(t, dir, "rev-parse", "HEAD")
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCmd(t, dir, "init", "--bare", remote)
	gitCmd(t, dir, "push", remote, published+":refs/heads/feature")
	gitCmd(t, dir, "remote", "set-url", "origin", remote)
	agentCommit(t, dir, "local.txt", "fix(tests): not published yet")

	if _, err := (&CIStep{}).commitRepair(sctx, "retry summary", nil); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD~1"); got != published {
		t.Fatalf("parent = %s, want the published commit %s kept as it is", got, published)
	}
	if got, want := fullMessage(t, dir, "HEAD"), "no-mistakes(ci): retry summary"; got != want {
		t.Fatalf("unpublished commit message = %q, want %q", got, want)
	}
	if got := fullMessage(t, dir, published); !strings.Contains(got, "already on the remote") {
		t.Fatalf("published commit message = %q, want it unchanged", got)
	}
}

func TestPublishedHeadBoundedContext_AddsDeadlineWithoutTouchingTheRunContext(t *testing.T) {
	t.Parallel()
	sctx, _, _ := agentCommitContext(t, nil)
	if _, ok := sctx.Ctx.Deadline(); ok {
		t.Fatal("test run context unexpectedly has a deadline")
	}
	bounded, cancel := publishedHeadBoundedContext(sctx)
	defer cancel()
	deadline, ok := bounded.Ctx.Deadline()
	if !ok {
		t.Fatal("remote read has no deadline, so a stalled connection would block the repair")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > defaultPublishedHeadResolveWindow {
		t.Fatalf("deadline in %s, want within %s", remaining, defaultPublishedHeadResolveWindow)
	}
	if _, ok := sctx.Ctx.Deadline(); ok {
		t.Fatal("the run context must keep no deadline")
	}
}
