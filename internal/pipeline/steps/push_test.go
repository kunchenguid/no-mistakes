package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

func TestPushStep_ReconcilesStaleDatabaseHeadSHA(t *testing.T) {
	t.Parallel()
	// When push retries after a prior UpdateRunHeadSHA failure, there are no
	// uncommitted changes. The step must still reconcile the DB if HeadSHA is stale.
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
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	actualHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	baseSHA := gitCmd(t, dir, "rev-parse", "main")
	gitCmd(t, dir, "push", "origin", "feature")

	// Create context with a stale HeadSHA (simulates prior DB write failure)
	staleHeadSHA := baseSHA // intentionally wrong
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, staleHeadSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream

	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, actualHeadSHA, "pre_verify"); err != nil {
		t.Fatal(err)
	}
	step := &PushStep{}
	_, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	// In-memory HeadSHA must match actual HEAD
	if sctx.Run.HeadSHA != actualHeadSHA {
		t.Errorf("Run.HeadSHA = %s, want %s", sctx.Run.HeadSHA, actualHeadSHA)
	}

	// DB record must also be updated
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.HeadSHA != actualHeadSHA {
		t.Errorf("DB HeadSHA = %s, want %s", dbRun.HeadSHA, actualHeadSHA)
	}
}

func TestPushStep_TargetsForkWhenConfigured(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	fork := t.TempDir()
	gitCmd(t, parent, "init", "--bare")
	gitCmd(t, fork, "init", "--bare")

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
	gitCmd(t, dir, "remote", "add", "origin", parent)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", fork, "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = parent
	sctx.Repo.ForkURL = fork
	sctx.Run.Branch = "feature"

	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, headSHA, "pre_verify"); err != nil {
		t.Fatal(err)
	}
	step := &PushStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	forkSHA := gitCmd(t, fork, "rev-parse", "refs/heads/feature")
	if forkSHA != headSHA {
		t.Fatalf("fork branch SHA = %s, want %s", forkSHA, headSHA)
	}
	if out, err := exec.Command("git", "-C", parent, "rev-parse", "--verify", "refs/heads/feature").CombinedOutput(); err == nil {
		t.Fatalf("parent unexpectedly received feature branch at %s", strings.TrimSpace(string(out)))
	}
}

func TestPushStep_RedactsForkURLInGitErrors(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "git-remote-error")
	t.Setenv("FAKE_CLI_REAL_GIT", realGit)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = "https://github.com/parent/project.git"
	sctx.Repo.ForkURL = "https://user:secret@example.com/fork/project.git"
	sctx.Run.Branch = "refs/heads/feature"

	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, headSHA, "pre_verify"); err != nil {
		t.Fatal(err)
	}
	step := &PushStep{}
	_, err = step.Execute(sctx)
	if err == nil {
		t.Fatal("expected push error")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("expected error to redact fork credentials, got %v", err)
	}
	if !strings.Contains(err.Error(), "https://redacted@example.com/fork/project.git") {
		t.Fatalf("expected redacted fork URL in error, got %v", err)
	}
}

// TestPushStep_RefusesWithoutSeal proves transport-only Push fails closed when
// no candidate has been sealed.
func TestPushStep_RefusesWithoutSeal(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	step := &PushStep{}
	if _, err := step.Execute(sctx); err == nil {
		t.Fatal("expected push to fail closed with no sealed candidate")
	}
}

// TestPushStep_RefusesDirtyWorktree proves Push refuses to publish a dirty
// worktree even though the sealed commit is unchanged.
func TestPushStep_RefusesDirtyWorktree(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, headSHA, "pre_verify"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}
	step := &PushStep{}
	if _, err := step.Execute(sctx); err == nil {
		t.Fatal("expected push to refuse a dirty worktree")
	}
}

// TestPushStep_RefusesChangedHead proves Push refuses when HEAD advanced past
// the sealed SHA, so a reverified candidate must be resealed before publishing.
func TestPushStep_RefusesChangedHead(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, headSHA, "pre_verify"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "after.txt"), []byte("later"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "post-seal change")
	step := &PushStep{}
	if _, err := step.Execute(sctx); err == nil {
		t.Fatal("expected push to refuse a HEAD that no longer matches the seal")
	}
}

func TestPushStep_PublishesSealedSHAWhenHeadMovesBeforeNormalPush(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, sealedSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "raced.txt"), []byte("not verified"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "unsealed race winner")
	racedSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "reset", "--hard", sealedSHA)
	armHeadMoveOnNextPush(t, racedSHA)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, baseSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, sealedSHA, "pre_verify"); err != nil {
		t.Fatal(err)
	}

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != sealedSHA {
		t.Fatalf("reconciled HEAD = %s, want sealed SHA %s (race moved it to %s)", got, sealedSHA, racedSHA)
	}
	if got := gitCmd(t, dir, "status", "--porcelain=v1", "--untracked-files=all"); got != "" {
		t.Fatalf("reconciled worktree is dirty: %q", got)
	}
	publishedSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if publishedSHA != sealedSHA {
		t.Fatalf("published SHA = %s, want sealed SHA %s", publishedSHA, sealedSHA)
	}
	if sctx.Run.HeadSHA != publishedSHA {
		t.Fatalf("Run.HeadSHA = %s, want published SHA %s", sctx.Run.HeadSHA, publishedSHA)
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.HeadSHA != publishedSHA {
		t.Fatalf("DB HeadSHA = %s, want published SHA %s", dbRun.HeadSHA, publishedSHA)
	}
}

func TestPushStep_PublishesSealedSHAWhenHeadMovesBeforeForcePush(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir, baseSHA, previousSHA := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "feature")

	gitCmd(t, dir, "reset", "--hard", baseSHA)
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("verified rewrite"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "sealed rewrite")
	sealedSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(dir, "raced.txt"), []byte("not verified"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "unsealed race winner")
	racedSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "reset", "--hard", sealedSHA)
	armHeadMoveOnNextPush(t, racedSHA)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, previousSHA, previousSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, sealedSHA, "pre_verify"); err != nil {
		t.Fatal(err)
	}

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != sealedSHA {
		t.Fatalf("reconciled HEAD = %s, want sealed SHA %s (race moved it to %s)", got, sealedSHA, racedSHA)
	}
	if got := gitCmd(t, dir, "status", "--porcelain=v1", "--untracked-files=all"); got != "" {
		t.Fatalf("reconciled worktree is dirty: %q", got)
	}
	publishedSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if publishedSHA != sealedSHA {
		t.Fatalf("published SHA = %s, want sealed SHA %s", publishedSHA, sealedSHA)
	}
	if sctx.Run.HeadSHA != publishedSHA {
		t.Fatalf("Run.HeadSHA = %s, want published SHA %s", sctx.Run.HeadSHA, publishedSHA)
	}
	dbRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbRun.HeadSHA != publishedSHA {
		t.Fatalf("DB HeadSHA = %s, want published SHA %s", dbRun.HeadSHA, publishedSHA)
	}
}

func TestPushStepPersistsExactTransactionBeforeTransport(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir, baseSHA, sealedSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, sealedSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "feature"
	seal, err := sctx.DB.CreateSeal(sctx.Run.ID, sealedSHA, "reviewed")
	if err != nil {
		t.Fatal(err)
	}

	transported := false
	step := &PushStep{transport: func(_ context.Context, _, _, _, _, _ string, _ bool) error {
		transported = true
		publication, err := sctx.DB.LatestPublication(sctx.Run.ID, db.PublicationKindPush)
		if err != nil {
			return err
		}
		if publication == nil {
			return errors.New("transport began without a durable publication")
		}
		if publication.State != db.PublicationStateAttempted ||
			publication.SealID != seal.ID ||
			publication.SealSHA != sealedSHA ||
			publication.DestinationURL != upstream ||
			publication.DestinationRef != "refs/heads/feature" ||
			publication.ExpectedRemoteSHA != "" ||
			!publication.Force {
			return errors.New("durable publication does not pin the exact transport")
		}
		return errors.New("injected transport rejection")
	}}

	if _, err := step.Execute(sctx); err == nil || !strings.Contains(err.Error(), "injected transport rejection") {
		t.Fatalf("Execute error = %v, want injected transport rejection", err)
	}
	if !transported {
		t.Fatal("transport seam was not reached")
	}
}

func TestPushStepExpectedAbsentLeaseRejectsConcurrentRemoteCreation(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir, baseSHA, sealedSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, sealedSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "race"
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, sealedSHA, "reviewed"); err != nil {
		t.Fatal(err)
	}

	step := &PushStep{transport: func(ctx context.Context, workDir, destinationURL, sourceSHA, destinationRef, expectedRemoteSHA string, force bool) error {
		gitCmd(t, workDir, "push", destinationURL, baseSHA+":"+destinationRef)
		return git.Push(ctx, workDir, destinationURL, sourceSHA, destinationRef, expectedRemoteSHA, force)
	}}
	if _, err := step.Execute(sctx); err == nil || !strings.Contains(err.Error(), "stale info") {
		t.Fatalf("Execute error = %v, want expected-absent lease rejection", err)
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/race"); got != baseSHA {
		t.Fatalf("concurrently created remote ref = %s, want preserved %s", got, baseSHA)
	}
	publication, err := sctx.DB.LatestPublication(sctx.Run.ID, db.PublicationKindPush)
	if err != nil {
		t.Fatal(err)
	}
	if publication == nil || publication.ExpectedRemoteSHA != "" || !publication.Force || publication.State != db.PublicationStateAttempted {
		t.Fatalf("rejected expected-absent publication = %+v", publication)
	}
}

func TestPushStepReconcilesAcceptedAmbiguousTransportWithoutRepush(t *testing.T) {
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir, baseSHA, sealedSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, sealedSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "feature"
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, sealedSHA, "reviewed"); err != nil {
		t.Fatal(err)
	}

	calls := 0
	step := &PushStep{transport: func(ctx context.Context, workDir, pushURL, sourceRef, destinationRef, expectedRemoteSHA string, force bool) error {
		calls++
		if err := git.Push(ctx, workDir, pushURL, sourceRef, destinationRef, expectedRemoteSHA, force); err != nil {
			return err
		}
		return errors.New("connection lost after server accepted push")
	}}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if calls != 1 {
		t.Fatalf("transport calls = %d, want exactly 1", calls)
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != sealedSHA {
		t.Fatalf("remote SHA = %s, want sealed SHA %s", got, sealedSHA)
	}
	publication, err := sctx.DB.LatestPublication(sctx.Run.ID, db.PublicationKindPush)
	if err != nil {
		t.Fatal(err)
	}
	if publication == nil || publication.State != db.PublicationStateCompleted {
		t.Fatalf("publication = %+v, want completed ambiguous transport", publication)
	}
}
func TestPushStepRetryUsesPersistedDestinationAfterRepoMetadataChanges(t *testing.T) {
	original := t.TempDir()
	reconfigured := t.TempDir()
	gitCmd(t, original, "init", "--bare")
	gitCmd(t, reconfigured, "init", "--bare")
	dir, baseSHA, sealedSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, sealedSHA, config.Commands{})
	sctx.Repo.UpstreamURL = original
	sctx.Run.Branch = "feature"
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, sealedSHA, "reviewed"); err != nil {
		t.Fatal(err)
	}

	step := &PushStep{transport: func(_ context.Context, _, destinationURL, _, _, _ string, _ bool) error {
		if destinationURL != original {
			return fmt.Errorf("first destination = %s, want %s", destinationURL, original)
		}
		return errors.New("injected rejection before acceptance")
	}}
	if _, err := step.Execute(sctx); err == nil {
		t.Fatal("expected first transport to fail")
	}
	sctx.Repo.UpstreamURL = reconfigured
	step.transport = func(ctx context.Context, workDir, destinationURL, sourceSHA, destinationRef, expectedRemoteSHA string, force bool) error {
		if destinationURL != original {
			return fmt.Errorf("retried destination = %s, want persisted %s", destinationURL, original)
		}
		return git.Push(ctx, workDir, destinationURL, sourceSHA, destinationRef, expectedRemoteSHA, force)
	}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, original, "rev-parse", "refs/heads/feature"); got != sealedSHA {
		t.Fatalf("original destination SHA = %s, want %s", got, sealedSHA)
	}
	if output, err := exec.Command("git", "-C", reconfigured, "rev-parse", "--verify", "refs/heads/feature").CombinedOutput(); err == nil {
		t.Fatalf("reconfigured destination unexpectedly received %s", strings.TrimSpace(string(output)))
	}
}

func armHeadMoveOnNextPush(t *testing.T, raceSHA string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "git-move-head-on-push")
	t.Setenv("FAKE_CLI_REAL_GIT", realGit)
	t.Setenv("FAKE_CLI_RACE_SHA", raceSHA)
}
