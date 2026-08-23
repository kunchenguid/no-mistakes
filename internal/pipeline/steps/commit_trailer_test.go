package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/committrailer"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func testCommitTrailers(t *testing.T) []committrailer.Trailer {
	t.Helper()
	trailers, err := committrailer.ParseMany([]string{
		"Co-Authored-By: Phiora Agent <agent@phiora.test>",
		"Reviewed-by: Reviewer <reviewer@phiora.test>",
		"Co-Authored-By: Phiora Agent <agent@phiora.test>",
	})
	if err != nil {
		t.Fatal(err)
	}
	return trailers
}

func assertTrailerMessage(t *testing.T, dir, subject string) {
	t.Helper()
	message := gitCmd(t, dir, "log", "-1", "--format=%B")
	want := subject + "\n\n" +
		"Co-Authored-By: Phiora Agent <agent@phiora.test>\n" +
		"Reviewed-by: Reviewer <reviewer@phiora.test>"
	if message != want {
		t.Fatalf("commit message = %q, want %q", message, want)
	}
	if got := strings.Count(message, "Co-Authored-By: Phiora Agent <agent@phiora.test>"); got != 1 {
		t.Fatalf("co-author trailer count = %d in %q, want 1", got, message)
	}
}

func TestCommitAgentFixesAddsRunCommitTrailers(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "agent-fix.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.CommitTrailers = testCommitTrailers(t)

	if err := commitAgentFixes(sctx, types.StepTest, "apply test fix", "fallback"); err != nil {
		t.Fatal(err)
	}
	assertTrailerMessage(t, dir, "no-mistakes(test): apply test fix")

	if err := os.WriteFile(filepath.Join(dir, "agent-fix-2.txt"), []byte("fixed again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := commitAgentFixes(sctx, types.StepTest, "apply another test fix", "fallback"); err != nil {
		t.Fatal(err)
	}
	assertTrailerMessage(t, dir, "no-mistakes(test): apply another test fix")
}

func TestCommitPipelineCorrectionAddsRunCommitTrailers(t *testing.T) {
	t.Parallel()
	dir, _, _ := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "leftover.txt"), []byte("leftover\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	if err := commitPipelineCorrection(
		context.Background(),
		dir,
		"no-mistakes: apply agent fixes",
		nil,
		testCommitTrailers(t),
	); err != nil {
		t.Fatal(err)
	}
	assertTrailerMessage(t, dir, "no-mistakes: apply agent fixes")
}

func TestCommitPipelineCorrectionTreatsTrailerAsArgumentVector(t *testing.T) {
	t.Parallel()
	dir, _, _ := setupGitRepo(t)
	sentinel := filepath.Join(t.TempDir(), "shell-injection")
	if err := os.WriteFile(filepath.Join(dir, "leftover.txt"), []byte("leftover\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	trailers, err := committrailer.ParseMany([]string{
		"Co-Authored-By: Phiora Agent <agent@phiora.test>; touch " + sentinel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := commitPipelineCorrection(
		context.Background(),
		dir,
		"no-mistakes: apply agent fixes",
		nil,
		trailers,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("shell metacharacters were interpreted; stat err = %v", err)
	}
	message := gitCmd(t, dir, "log", "-1", "--format=%B")
	if !strings.Contains(message, trailers[0].String()) {
		t.Fatalf("commit message %q does not contain literal trailer %q", message, trailers[0].String())
	}
}

func TestCIStepCommitAndPushAddsRunCommitTrailers(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

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
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("ci fix"), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.CommitTrailers = testCommitTrailers(t)
	if err := sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{HeadSHA: headSHA, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(upstream), Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}

	pushed, err := (&CIStep{}).commitAndPush(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !pushed {
		t.Fatal("expected CI commit to push")
	}
	assertTrailerMessage(t, dir, "no-mistakes: apply CI fixes")
}

func TestPushStepAddsRunCommitTrailersToLeftoverCommit(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

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
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	baseSHA := gitCmd(t, dir, "rev-parse", "main")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "leftover.txt"), []byte("leftover\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "feature"
	sctx.Run.CommitTrailers = testCommitTrailers(t)
	recordReviewApproval(t, sctx, headSHA)

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	assertTrailerMessage(t, dir, "no-mistakes: apply agent fixes")
}
