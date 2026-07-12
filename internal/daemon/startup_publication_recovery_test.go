package daemon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type startupPublicationFixture struct {
	paths     *paths.Paths
	database  *db.DB
	repo      *db.Repo
	run       *db.Run
	worktree  string
	remote    string
	baseSHA   string
	sealedSHA string
	seal      *db.Seal
}

func newStartupPublicationFixture(t *testing.T, name string) startupPublicationFixture {
	t.Helper()

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	source := filepath.Join(t.TempDir(), "source")
	gitCmd(t, "", "init", source)
	gitCmd(t, source, "config", "user.email", "test@example.com")
	gitCmd(t, source, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(source, "candidate.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, source, "add", "candidate.txt")
	gitCmd(t, source, "commit", "-m", "base")
	baseSHA := gitOutput(t, source, "rev-parse", "HEAD")

	remote := filepath.Join(t.TempDir(), "publication.git")
	gitCmd(t, "", "init", "--bare", remote)
	gitCmd(t, remote, "config", "core.logAllRefUpdates", "true")
	gitCmd(t, source, "push", remote, baseSHA+":refs/heads/pinned")

	if err := os.WriteFile(filepath.Join(source, "candidate.txt"), []byte("sealed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, source, "add", "candidate.txt")
	gitCmd(t, source, "commit", "-m", "sealed")
	sealedSHA := gitOutput(t, source, "rev-parse", "HEAD")

	repoID := "publication-" + name
	gate := p.RepoDir(repoID)
	gitCmd(t, "", "init", "--bare", gate)
	gitCmd(t, source, "push", gate, sealedSHA+":refs/heads/feature")
	repo, err := database.InsertRepoWithID(repoID, source, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", baseSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	run.Status = types.RunRunning
	worktree := p.WorktreeDir(repo.ID, run.ID)
	if err := gitpkg.WorktreeAdd(context.Background(), gate, worktree, baseSHA); err != nil {
		t.Fatal(err)
	}
	seal, err := database.CreateSeal(run.ID, sealedSHA, "reviewed")
	if err != nil {
		t.Fatal(err)
	}

	return startupPublicationFixture{
		paths: p, database: database, repo: repo, run: run, worktree: worktree,
		remote: remote, baseSHA: baseSHA, sealedSHA: sealedSHA, seal: seal,
	}
}

func (f startupPublicationFixture) dirtyWorktree(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.worktree, "candidate.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, "staged.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.worktree, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(f.worktree, "untracked.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f startupPublicationFixture) assertSealedCleanState(t *testing.T) {
	t.Helper()
	if got := gitOutput(t, f.worktree, "rev-parse", "HEAD"); got != f.sealedSHA {
		t.Fatalf("worktree HEAD = %s, want sealed SHA %s", got, f.sealedSHA)
	}
	if got := gitOutput(t, f.worktree, "write-tree"); got != gitOutput(t, f.worktree, "rev-parse", f.sealedSHA+"^{tree}") {
		t.Fatalf("worktree index tree = %s, want sealed tree", got)
	}
	if got := strings.TrimSpace(gitOutputAllowEmpty(t, f.worktree, "status", "--porcelain=v1", "--untracked-files=all")); got != "" {
		t.Fatalf("worktree is not clean after publication recovery: %q", got)
	}
}

func gitOutputAllowEmpty(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitpkg.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v failed: %v", args, err)
	}
	return out
}

func (f startupPublicationFixture) assertRemoteSealedOnce(t *testing.T) {
	t.Helper()
	if got := gitOutput(t, f.remote, "rev-parse", "refs/heads/pinned"); got != f.sealedSHA {
		t.Fatalf("remote pinned ref = %s, want sealed SHA %s", got, f.sealedSHA)
	}
	reflog := strings.Fields(gitOutput(t, f.remote, "reflog", "show", "--format=%H", "refs/heads/pinned"))
	count := 0
	for _, sha := range reflog {
		if sha == f.sealedSHA {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("remote accepted sealed SHA %d times, want exactly once (reflog %v)", count, reflog)
	}
}

func legacyCIRepublishPendingRef(runID string) string {
	sum := sha256.Sum256([]byte(runID))
	return fmt.Sprintf("refs/no-mistakes/ci-republish-pending/%x", sum[:])
}

type publicationMutationSnapshot struct {
	head       string
	status     string
	refs       string
	sentinel   string
	remoteRefs string
}

func capturePublicationMutationSnapshot(t *testing.T, worktree, remote string) publicationMutationSnapshot {
	t.Helper()
	sentinel, err := os.ReadFile(filepath.Join(worktree, "sentinel.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return publicationMutationSnapshot{
		head:       gitOutput(t, worktree, "rev-parse", "HEAD"),
		status:     gitOutputAllowEmpty(t, worktree, "status", "--porcelain=v1", "--untracked-files=all"),
		refs:       gitOutputAllowEmpty(t, worktree, "for-each-ref", "--format=%(refname):%(objectname)"),
		sentinel:   string(sentinel),
		remoteRefs: gitOutputAllowEmpty(t, remote, "for-each-ref", "--format=%(refname):%(objectname)"),
	}
}

func assertPublicationMutationSnapshot(t *testing.T, worktree, remote string, want publicationMutationSnapshot) {
	t.Helper()
	if got := capturePublicationMutationSnapshot(t, worktree, remote); got != want {
		t.Fatalf("untrusted checkout mutated during rejected startup:\n got: %+v\nwant: %+v", got, want)
	}
}

func initializeForeignPublicationCheckout(t *testing.T, dir string) {
	t.Helper()
	gitCmd(t, "", "init", dir)
	gitCmd(t, dir, "config", "user.email", "test@example.com")
	gitCmd(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "sentinel.txt"), []byte("foreign baseline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "sentinel.txt")
	gitCmd(t, dir, "commit", "-m", "foreign baseline")
	if err := os.WriteFile(filepath.Join(dir, "sentinel.txt"), []byte("foreign dirty: do not reset\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "update-ref", "refs/no-mistakes/sentinel", "HEAD")
}

func replacePublicationWorktree(t *testing.T, fixture startupPublicationFixture, replacement string) string {
	t.Helper()
	switch replacement {
	case "worktree-symlink":
		foreign := filepath.Join(t.TempDir(), "foreign")
		initializeForeignPublicationCheckout(t, foreign)
		if err := os.RemoveAll(fixture.worktree); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(foreign, fixture.worktree); err != nil {
			t.Fatal(err)
		}
		return foreign
	case "escaped-parent-symlink":
		repoWorktrees := filepath.Dir(fixture.worktree)
		escaped := filepath.Join(t.TempDir(), "escaped")
		if err := os.Rename(repoWorktrees, escaped); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(escaped, repoWorktrees); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture.worktree, "sentinel.txt"), []byte("escaped dirty: do not reset\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, fixture.worktree, "update-ref", "refs/no-mistakes/sentinel", "HEAD")
		return fixture.worktree
	case "foreign-checkout":
		if err := os.RemoveAll(fixture.worktree); err != nil {
			t.Fatal(err)
		}
		initializeForeignPublicationCheckout(t, fixture.worktree)
		return fixture.worktree
	default:
		t.Fatalf("unknown worktree replacement %q", replacement)
		return ""
	}
}

func (f startupPublicationFixture) assertLegacyCIRecovery(t *testing.T, stepID, pendingRef string) {
	t.Helper()

	gotRun, err := f.database.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != types.RunRunning || gotRun.HeadSHA != f.sealedSHA {
		t.Fatalf("run after legacy CI recovery = status %s head %s, want running at %s", gotRun.Status, gotRun.HeadSHA, f.sealedSHA)
	}
	active, err := f.database.GetActiveRun(f.repo.ID, f.run.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if active == nil || active.ID != f.run.ID {
		t.Fatalf("active run after legacy CI recovery = %+v, want %s", active, f.run.ID)
	}
	gotStep, err := f.database.GetStepResult(stepID)
	if err != nil {
		t.Fatal(err)
	}
	if gotStep.Status != types.StepStatusPending {
		t.Fatalf("CI status after legacy recovery = %s, want pending", gotStep.Status)
	}
	publication, err := f.database.LatestPublication(f.run.ID, db.PublicationKindCI)
	if err != nil {
		t.Fatal(err)
	}
	if publication == nil ||
		publication.State != db.PublicationStateCompleted ||
		publication.SealSHA != f.sealedSHA ||
		publication.DestinationURL != f.remote ||
		publication.DestinationRef != "refs/heads/feature" {
		t.Fatalf("legacy CI publication = %+v, want completed exact candidate %s to feature", publication, f.sealedSHA)
	}
	seal, err := f.database.LatestSealByReason(f.run.ID, "ci_republish")
	if err != nil {
		t.Fatal(err)
	}
	if seal == nil || seal.ID != publication.SealID || seal.SHA != f.sealedSHA {
		t.Fatalf("legacy CI seal = %+v, want journal seal %s at %s", seal, publication.SealID, f.sealedSHA)
	}
	if got := gitOutput(t, f.remote, "rev-parse", "refs/heads/feature"); got != f.sealedSHA {
		t.Fatalf("remote feature ref = %s, want exact recovered candidate %s", got, f.sealedSHA)
	}
	if got := strings.TrimSpace(gitOutputAllowEmpty(t, f.worktree, "for-each-ref", "--format=%(objectname)", pendingRef)); got != "" {
		t.Fatalf("legacy pending ref still points to %s", got)
	}
	f.assertSealedCleanState(t)
}

func TestRecoverOnStartup_PublicationRecoveryUpgradesLegacyCIProtectedRefBeforeStaleCleanup(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	fixture := newStartupPublicationFixture(t, "legacy-ci-ref")
	fixture.dirtyWorktree(t)

	step, err := fixture.database.InsertStepResult(fixture.run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	pendingRef := legacyCIRepublishPendingRef(fixture.run.ID)
	gitCmd(t, fixture.worktree, "update-ref", pendingRef, fixture.sealedSHA)

	if publication, err := fixture.database.LatestPublication(fixture.run.ID, db.PublicationKindCI); err != nil {
		t.Fatal(err)
	} else if publication != nil {
		t.Fatalf("legacy protected-ref fixture already has publication %+v", publication)
	}
	if seal, err := fixture.database.LatestSealByReason(fixture.run.ID, "ci_republish"); err != nil {
		t.Fatal(err)
	} else if seal != nil {
		t.Fatalf("legacy protected-ref fixture already has CI seal %+v", seal)
	}

	manager := NewRunManager(fixture.database, fixture.paths, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepCI}}
	})
	manager.Shutdown()

	if err := recoverOnStartup(fixture.database, fixture.paths, manager); err != nil {
		t.Fatalf("recoverOnStartup: %v", err)
	}
	fixture.assertLegacyCIRecovery(t, step.ID, pendingRef)
}

func TestRecoverOnStartup_PublicationRecoveryUpgradesLegacyCISealBeforeStaleCleanup(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	fixture := newStartupPublicationFixture(t, "legacy-ci-seal")
	fixture.dirtyWorktree(t)

	step, err := fixture.database.InsertStepResult(fixture.run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	seal, err := fixture.database.CreateSeal(fixture.run.ID, fixture.sealedSHA, "ci_republish")
	if err != nil {
		t.Fatal(err)
	}
	pendingRef := legacyCIRepublishPendingRef(fixture.run.ID)
	if got := strings.TrimSpace(gitOutputAllowEmpty(t, fixture.worktree, "for-each-ref", "--format=%(objectname)", pendingRef)); got != "" {
		t.Fatalf("legacy seal-only fixture already has protected ref %s", got)
	}
	if publication, err := fixture.database.LatestPublication(fixture.run.ID, db.PublicationKindCI); err != nil {
		t.Fatal(err)
	} else if publication != nil {
		t.Fatalf("legacy seal-only fixture already has publication %+v", publication)
	}

	manager := NewRunManager(fixture.database, fixture.paths, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepCI}}
	})
	manager.Shutdown()

	if err := recoverOnStartup(fixture.database, fixture.paths, manager); err != nil {
		t.Fatalf("recoverOnStartup: %v", err)
	}
	fixture.assertLegacyCIRecovery(t, step.ID, pendingRef)
	recoveredSeal, err := fixture.database.LatestSealByReason(fixture.run.ID, "ci_republish")
	if err != nil {
		t.Fatal(err)
	}
	if recoveredSeal.ID != seal.ID {
		t.Fatalf("legacy seal ID after recovery = %s, want existing seal %s", recoveredSeal.ID, seal.ID)
	}
}
func TestRecoverOnStartup_PublicationRecoveryUpgradesUpToDateLegacyCISealWithoutTransport(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	fixture := newStartupPublicationFixture(t, "legacy-ci-seal-up-to-date")
	gitCmd(t, fixture.worktree, "push", fixture.remote, fixture.sealedSHA+":refs/heads/feature")
	if err := fixture.database.UpdateRunHeadSHA(fixture.run.ID, fixture.sealedSHA); err != nil {
		t.Fatal(err)
	}
	fixture.run.HeadSHA = fixture.sealedSHA
	fixture.dirtyWorktree(t)

	step, err := fixture.database.InsertStepResult(fixture.run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	seal, err := fixture.database.CreateSeal(fixture.run.ID, fixture.sealedSHA, "ci_republish")
	if err != nil {
		t.Fatal(err)
	}
	pendingRef := legacyCIRepublishPendingRef(fixture.run.ID)
	beforeReflog := strings.Fields(gitOutput(t, fixture.remote, "reflog", "show", "--format=%H", "refs/heads/feature"))

	manager := NewRunManager(fixture.database, fixture.paths, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepCI}}
	})
	manager.Shutdown()

	if err := recoverOnStartup(fixture.database, fixture.paths, manager); err != nil {
		t.Fatalf("recoverOnStartup: %v", err)
	}
	fixture.assertLegacyCIRecovery(t, step.ID, pendingRef)
	publication, err := fixture.database.LatestPublication(fixture.run.ID, db.PublicationKindCI)
	if err != nil {
		t.Fatal(err)
	}
	if publication.SealID != seal.ID ||
		publication.ExpectedRemoteSHA != fixture.sealedSHA ||
		publication.Force {
		t.Fatalf("up-to-date legacy publication = %+v, want existing seal %s and no-op lease at %s", publication, seal.ID, fixture.sealedSHA)
	}
	afterReflog := strings.Fields(gitOutput(t, fixture.remote, "reflog", "show", "--format=%H", "refs/heads/feature"))
	if len(afterReflog) != len(beforeReflog) {
		t.Fatalf("up-to-date legacy recovery transported again: reflog grew from %v to %v", beforeReflog, afterReflog)
	}
	for i := range beforeReflog {
		if afterReflog[i] != beforeReflog[i] {
			t.Fatalf("up-to-date legacy recovery changed remote reflog from %v to %v", beforeReflog, afterReflog)
		}
	}
}

func TestRecoverOnStartup_PublicationRecoveryReconcilesAcceptedPushBeforeStaleCleanup(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	for _, state := range []db.PublicationState{
		db.PublicationStatePrepared,
		db.PublicationStateAttempted,
		db.PublicationStateAccepted,
		db.PublicationStateCompleted,
	} {
		state := state
		t.Run(string(state), func(t *testing.T) {
			fixture := newStartupPublicationFixture(t, "push-"+string(state))
			if state != db.PublicationStatePrepared {
				gitCmd(t, fixture.worktree, "push", fixture.remote, fixture.sealedSHA+":refs/heads/pinned")
			}
			fixture.dirtyWorktree(t)

			step, err := fixture.database.InsertStepResult(fixture.run.ID, types.StepPush)
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.database.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			publication, err := fixture.database.PreparePublication(db.PreparePublicationInput{
				RunID: fixture.run.ID, Kind: db.PublicationKindPush, SealID: fixture.seal.ID,
				SealSHA: fixture.sealedSHA, DestinationURL: fixture.remote,
				DestinationRef: "refs/heads/pinned", ExpectedRemoteSHA: fixture.baseSHA,
			})
			if err != nil {
				t.Fatal(err)
			}
			if state != db.PublicationStatePrepared {
				if err := fixture.database.MarkPublicationAttempted(publication.ID); err != nil {
					t.Fatal(err)
				}
			}
			if state == db.PublicationStateAccepted || state == db.PublicationStateCompleted {
				if err := fixture.database.MarkPublicationAccepted(publication.ID); err != nil {
					t.Fatal(err)
				}
			}
			if state == db.PublicationStateCompleted {
				if err := fixture.database.CompletePublication(publication.ID, db.PublicationRecoveryNone); err != nil {
					t.Fatal(err)
				}
			}

			manager := NewRunManager(fixture.database, fixture.paths, func() []pipeline.Step {
				return []pipeline.Step{&mockPassStep{name: types.StepPush}}
			})
			manager.Shutdown()

			if err := recoverOnStartup(fixture.database, fixture.paths, manager); err != nil {
				t.Fatalf("recoverOnStartup: %v", err)
			}

			gotRun, err := fixture.database.GetRun(fixture.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotRun.Status != types.RunRunning || gotRun.HeadSHA != fixture.sealedSHA {
				t.Fatalf("run after recovery = status %s head %s, want running at %s", gotRun.Status, gotRun.HeadSHA, fixture.sealedSHA)
			}
			gotStep, err := fixture.database.GetStepResult(step.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotStep.Status != types.StepStatusCompleted {
				t.Fatalf("Push status after recovery = %s, want completed", gotStep.Status)
			}
			fixture.assertSealedCleanState(t)
			fixture.assertRemoteSealedOnce(t)
		})
	}
}

func TestRecoverOnStartup_PublicationRecoveryReplaysPinnedCIBeforeStaleCleanup(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	for _, state := range []db.PublicationState{
		db.PublicationStatePrepared,
		db.PublicationStateAttempted,
		db.PublicationStateAccepted,
		db.PublicationStateCompleted,
	} {
		state := state
		t.Run(string(state), func(t *testing.T) {
			fixture := newStartupPublicationFixture(t, "ci-"+string(state))
			fixture.dirtyWorktree(t)

			step, err := fixture.database.InsertStepResult(fixture.run.ID, types.StepCI)
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.database.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			_, publication, err := fixture.database.PrepareCISealedPublication(db.PrepareCISealedPublicationInput{
				RunID: fixture.run.ID, SHA: fixture.sealedSHA,
				DestinationURL: fixture.remote, DestinationRef: "refs/heads/pinned",
				ExpectedRemoteSHA: fixture.baseSHA,
			})
			if err != nil {
				t.Fatal(err)
			}
			if state != db.PublicationStatePrepared {
				if err := fixture.database.MarkPublicationAttempted(publication.ID); err != nil {
					t.Fatal(err)
				}
			}
			if state == db.PublicationStateAccepted || state == db.PublicationStateCompleted {
				gitCmd(t, fixture.worktree, "push", fixture.remote, fixture.sealedSHA+":refs/heads/pinned")
				if err := fixture.database.MarkPublicationAccepted(publication.ID); err != nil {
					t.Fatal(err)
				}
			}
			if state == db.PublicationStateCompleted {
				if err := fixture.database.CompletePublication(publication.ID, db.PublicationRecoveryNone); err != nil {
					t.Fatal(err)
				}
			}

			manager := NewRunManager(fixture.database, fixture.paths, func() []pipeline.Step {
				return []pipeline.Step{&mockPassStep{name: types.StepCI}}
			})
			manager.Shutdown()

			if err := recoverOnStartup(fixture.database, fixture.paths, manager); err != nil {
				t.Fatalf("recoverOnStartup: %v", err)
			}

			gotRun, err := fixture.database.GetRun(fixture.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotRun.Status != types.RunRunning || gotRun.HeadSHA != fixture.sealedSHA {
				t.Fatalf("run after recovery = status %s head %s, want running at %s", gotRun.Status, gotRun.HeadSHA, fixture.sealedSHA)
			}
			gotStep, err := fixture.database.GetStepResult(step.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotStep.Status != types.StepStatusPending {
				t.Fatalf("CI status after recovery = %s, want pending", gotStep.Status)
			}
			gotPublication, err := fixture.database.GetPublication(publication.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotPublication.State != db.PublicationStateCompleted {
				t.Fatalf("CI publication state after recovery = %s, want completed", gotPublication.State)
			}
			fixture.assertSealedCleanState(t)
			fixture.assertRemoteSealedOnce(t)
		})
	}
}

func TestRecoverOnStartup_AcceptedCIPublicationRestoresDurableIgnoredSnapshotBeforeCompletion(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	fixture := newStartupPublicationFixture(t, "ci-accepted-cleanup")
	excludePath := gitOutput(t, fixture.worktree, "rev-parse", "--git-path", "info/exclude")
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(fixture.worktree, excludePath)
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(excludePath, []byte("ignored-local*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ignoredPath := filepath.Join(fixture.worktree, "ignored-local")
	if err := os.WriteFile(ignoredPath, []byte("original ignored state\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	gitDir := gitOutput(t, fixture.worktree, "rev-parse", "--absolute-git-dir")
	snapshotDir, err := os.MkdirTemp(gitDir, "no-mistakes-ci-candidate-")
	if err != nil {
		t.Fatal(err)
	}
	snapshotWorktree := filepath.Join(snapshotDir, "worktree")
	if err := os.Mkdir(snapshotWorktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(snapshotDir, "git-state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshotWorktree, "ignored-local"), []byte("original ignored state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, "publication-cleanup.json"), []byte(`{"ignored_paths":["ignored-local"]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(ignoredPath, []byte("repair-mutated ignored state\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.worktree, "ignored-local-created"), []byte("repair-created\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	step, err := fixture.database.InsertStepResult(fixture.run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	_, publication, err := fixture.database.PrepareCISealedPublication(db.PrepareCISealedPublicationInput{
		RunID: fixture.run.ID, SHA: fixture.sealedSHA,
		DestinationURL: fixture.remote, DestinationRef: "refs/heads/pinned",
		ExpectedRemoteSHA: fixture.baseSHA, CleanupSnapshotDir: snapshotDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.MarkPublicationAttempted(publication.ID); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, fixture.worktree, "push", fixture.remote, fixture.sealedSHA+":refs/heads/pinned")
	if err := fixture.database.MarkPublicationAccepted(publication.ID); err != nil {
		t.Fatal(err)
	}

	manager := NewRunManager(fixture.database, fixture.paths, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepCI}}
	})
	manager.Shutdown()
	if err := recoverOnStartup(fixture.database, fixture.paths, manager); err != nil {
		t.Fatalf("recoverOnStartup: %v", err)
	}

	gotPublication, err := fixture.database.GetPublication(publication.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotPublication.State != db.PublicationStateCompleted {
		t.Fatalf("publication state after cleanup recovery = %s, want completed", gotPublication.State)
	}
	if got, readErr := os.ReadFile(ignoredPath); readErr != nil || string(got) != "original ignored state\n" {
		t.Fatalf("ignored state after startup recovery = %q, %v; want exact original", got, readErr)
	}
	if info, statErr := os.Stat(ignoredPath); statErr != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ignored mode after startup recovery = %v, %v; want 0600", info, statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(fixture.worktree, "ignored-local-created")); !os.IsNotExist(statErr) {
		t.Fatalf("repair-created ignored path survived startup recovery: %v", statErr)
	}
	if _, statErr := os.Lstat(snapshotDir); !os.IsNotExist(statErr) {
		t.Fatalf("completed startup cleanup retained snapshot: %v", statErr)
	}
	if err := os.Mkdir(snapshotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, "partially-deleted"), []byte("orphan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.StartStep(step.ID); err != nil {
		t.Fatalf("restart CI step for completed-cleanup recovery: %v", err)
	}
	restartedManager := NewRunManager(fixture.database, fixture.paths, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepCI}}
	})
	restartedManager.Shutdown()
	if err := recoverOnStartup(fixture.database, fixture.paths, restartedManager); err != nil {
		t.Fatalf("recoverOnStartup after partial snapshot deletion: %v", err)
	}
	if _, statErr := os.Lstat(snapshotDir); !os.IsNotExist(statErr) {
		t.Fatalf("completed recovery retained partially deleted snapshot: %v", statErr)
	}
	if got, readErr := os.ReadFile(ignoredPath); readErr != nil || string(got) != "original ignored state\n" {
		t.Fatalf("completed recovery mutated restored ignored state = %q, %v", got, readErr)
	}
	fixture.assertSealedCleanState(t)
}

func TestRecoverOnStartup_PublicationRecoveryConflictAbortsBeforeStaleCleanup(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	fixture := newStartupPublicationFixture(t, "conflict")
	if err := os.WriteFile(filepath.Join(fixture.repo.WorkingPath, "candidate.txt"), []byte("remote moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, fixture.repo.WorkingPath, "add", "candidate.txt")
	gitCmd(t, fixture.repo.WorkingPath, "commit", "-m", "remote moved")
	movedSHA := gitOutput(t, fixture.repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, fixture.repo.WorkingPath, "push", fixture.remote, movedSHA+":refs/heads/pinned")

	step, err := fixture.database.InsertStepResult(fixture.run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	_, publication, err := fixture.database.PrepareCISealedPublication(db.PrepareCISealedPublicationInput{
		RunID: fixture.run.ID, SHA: fixture.sealedSHA,
		DestinationURL: fixture.remote, DestinationRef: "refs/heads/pinned",
		ExpectedRemoteSHA: fixture.baseSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.MarkPublicationAttempted(publication.ID); err != nil {
		t.Fatal(err)
	}

	manager := NewRunManager(fixture.database, fixture.paths, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepCI}}
	})
	defer manager.Shutdown()

	err = recoverOnStartup(fixture.database, fixture.paths, manager)
	if err == nil || !strings.Contains(err.Error(), "moved from expected") {
		t.Fatalf("recoverOnStartup conflict error = %v, want persisted lease conflict", err)
	}

	gotRun, err := fixture.database.GetRun(fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != types.RunRunning || gotRun.HeadSHA != fixture.baseSHA {
		t.Fatalf("run after rejected recovery = status %s head %s, want running at %s", gotRun.Status, gotRun.HeadSHA, fixture.baseSHA)
	}
	gotStep, err := fixture.database.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotStep.Status != types.StepStatusRunning {
		t.Fatalf("CI status after rejected recovery = %s, want running", gotStep.Status)
	}
	gotPublication, err := fixture.database.GetPublication(publication.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotPublication.State != db.PublicationStateAttempted {
		t.Fatalf("publication state after rejected recovery = %s, want attempted", gotPublication.State)
	}
	if got := gitOutput(t, fixture.remote, "rev-parse", "refs/heads/pinned"); got != movedSHA {
		t.Fatalf("remote ref after rejected recovery = %s, want conflicting SHA %s", got, movedSHA)
	}
}

func TestRecoverOnStartup_PublicationRecoveryMissingWorktreeAbortsBeforeStaleCleanup(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	fixture := newStartupPublicationFixture(t, "missing-worktree")
	step, err := fixture.database.InsertStepResult(fixture.run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.database.PreparePublication(db.PreparePublicationInput{
		RunID: fixture.run.ID, Kind: db.PublicationKindPush, SealID: fixture.seal.ID,
		SealSHA: fixture.sealedSHA, DestinationURL: fixture.remote,
		DestinationRef: "refs/heads/pinned", ExpectedRemoteSHA: fixture.baseSHA,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(fixture.worktree); err != nil {
		t.Fatal(err)
	}

	manager := NewRunManager(fixture.database, fixture.paths, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepPush}}
	})
	defer manager.Shutdown()

	err = recoverOnStartup(fixture.database, fixture.paths, manager)
	if err == nil || !strings.Contains(err.Error(), "inspect worktree") {
		t.Fatalf("recoverOnStartup missing-worktree error = %v, want closed failure", err)
	}
	gotRun, err := fixture.database.GetRun(fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != types.RunRunning {
		t.Fatalf("run after missing-worktree recovery = %s, want running", gotRun.Status)
	}
	gotStep, err := fixture.database.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotStep.Status != types.StepStatusRunning {
		t.Fatalf("Push after missing-worktree recovery = %s, want running", gotStep.Status)
	}
}
func TestRecoverOnStartup_PublicationRecoveryMissingWorktreeAbortsForUpToDateLegacySeal(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	fixture := newStartupPublicationFixture(t, "missing-legacy-seal-worktree")
	gitCmd(t, fixture.worktree, "push", fixture.remote, fixture.sealedSHA+":refs/heads/feature")
	if err := fixture.database.UpdateRunHeadSHA(fixture.run.ID, fixture.sealedSHA); err != nil {
		t.Fatal(err)
	}
	step, err := fixture.database.InsertStepResult(fixture.run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.database.CreateSeal(fixture.run.ID, fixture.sealedSHA, "ci_republish"); err != nil {
		t.Fatal(err)
	}
	remoteRefs := gitOutputAllowEmpty(t, fixture.remote, "for-each-ref", "--format=%(refname):%(objectname)")
	if err := os.RemoveAll(fixture.worktree); err != nil {
		t.Fatal(err)
	}

	manager := NewRunManager(fixture.database, fixture.paths, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepCI}}
	})
	manager.Shutdown()
	err = recoverOnStartup(fixture.database, fixture.paths, manager)
	if err == nil || !strings.Contains(err.Error(), "inspect worktree") {
		t.Fatalf("recoverOnStartup missing legacy worktree error = %v, want closed failure", err)
	}
	if got := gitOutputAllowEmpty(t, fixture.remote, "for-each-ref", "--format=%(refname):%(objectname)"); got != remoteRefs {
		t.Fatalf("remote refs changed before missing legacy worktree rejection:\n got %s\nwant %s", got, remoteRefs)
	}
	gotRun, err := fixture.database.GetRun(fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != types.RunRunning || gotRun.HeadSHA != fixture.sealedSHA {
		t.Fatalf("run after missing legacy worktree = status %s head %s, want running at %s", gotRun.Status, gotRun.HeadSHA, fixture.sealedSHA)
	}
	gotStep, err := fixture.database.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotStep.Status != types.StepStatusRunning {
		t.Fatalf("CI after missing legacy worktree = %s, want running", gotStep.Status)
	}
	if publication, err := fixture.database.LatestPublication(fixture.run.ID, db.PublicationKindCI); err != nil {
		t.Fatal(err)
	} else if publication != nil {
		t.Fatalf("missing legacy worktree created publication %+v", publication)
	}
}

func TestRecoverOnStartup_PublicationRecoveryRejectsUntrustedWorktreesBeforeMutation(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	for _, legacy := range []bool{false, true} {
		recoveryKind := "journal"
		if legacy {
			recoveryKind = "legacy"
		}
		for _, replacement := range []struct {
			name      string
			wantError string
		}{
			{name: "worktree-symlink", wantError: "symbolic link"},
			{name: "escaped-parent-symlink", wantError: "outside"},
			{name: "foreign-checkout", wantError: "does not belong"},
		} {
			replacement := replacement
			t.Run(recoveryKind+"/"+replacement.name, func(t *testing.T) {
				fixture := newStartupPublicationFixture(t, recoveryKind+"-"+replacement.name)
				fixture.dirtyWorktree(t)

				stepName := types.StepPush
				if legacy {
					stepName = types.StepCI
				}
				step, err := fixture.database.InsertStepResult(fixture.run.ID, stepName)
				if err != nil {
					t.Fatal(err)
				}
				if err := fixture.database.StartStep(step.ID); err != nil {
					t.Fatal(err)
				}
				var publication *db.Publication
				if legacy {
					if _, err := fixture.database.CreateSeal(fixture.run.ID, fixture.sealedSHA, "ci_republish"); err != nil {
						t.Fatal(err)
					}
				} else {
					publication, err = fixture.database.PreparePublication(db.PreparePublicationInput{
						RunID: fixture.run.ID, Kind: db.PublicationKindPush, SealID: fixture.seal.ID,
						SealSHA: fixture.sealedSHA, DestinationURL: fixture.remote,
						DestinationRef: "refs/heads/pinned", ExpectedRemoteSHA: fixture.baseSHA,
					})
					if err != nil {
						t.Fatal(err)
					}
				}

				untrustedCheckout := replacePublicationWorktree(t, fixture, replacement.name)
				before := capturePublicationMutationSnapshot(t, untrustedCheckout, fixture.remote)
				manager := NewRunManager(fixture.database, fixture.paths, func() []pipeline.Step {
					return []pipeline.Step{&mockPassStep{name: stepName}}
				})
				manager.Shutdown()

				err = recoverOnStartup(fixture.database, fixture.paths, manager)
				if err == nil || !strings.Contains(err.Error(), replacement.wantError) {
					t.Fatalf("recoverOnStartup error = %v, want closed failure containing %q", err, replacement.wantError)
				}
				assertPublicationMutationSnapshot(t, untrustedCheckout, fixture.remote, before)

				gotRun, err := fixture.database.GetRun(fixture.run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if gotRun.Status != types.RunRunning || gotRun.HeadSHA != fixture.baseSHA {
					t.Fatalf("run after rejected recovery = status %s head %s, want running at %s", gotRun.Status, gotRun.HeadSHA, fixture.baseSHA)
				}
				gotStep, err := fixture.database.GetStepResult(step.ID)
				if err != nil {
					t.Fatal(err)
				}
				if gotStep.Status != types.StepStatusRunning {
					t.Fatalf("%s after rejected recovery = %s, want running", stepName, gotStep.Status)
				}
				gotPublication, err := fixture.database.LatestPublication(fixture.run.ID, db.PublicationKindPush)
				if err != nil {
					t.Fatal(err)
				}
				if legacy {
					if gotPublication != nil {
						t.Fatalf("legacy rejected recovery created publication %+v", gotPublication)
					}
				} else if gotPublication == nil ||
					gotPublication.ID != publication.ID ||
					gotPublication.State != db.PublicationStatePrepared {
					t.Fatalf("journal after rejected recovery = %+v, want prepared publication %s", gotPublication, publication.ID)
				}
			})
		}
	}
}

func TestRecoverOnStartup_PublicationRecoveryAllowsMissingWorktreeForOrdinaryStaleRun(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	fixture := newStartupPublicationFixture(t, "ordinary-stale")
	step, err := fixture.database.InsertStepResult(fixture.run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(fixture.worktree); err != nil {
		t.Fatal(err)
	}

	manager := NewRunManager(fixture.database, fixture.paths, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepPush}}
	})
	manager.Shutdown()
	if err := recoverOnStartup(fixture.database, fixture.paths, manager); err != nil {
		t.Fatalf("recoverOnStartup ordinary stale run: %v", err)
	}

	gotRun, err := fixture.database.GetRun(fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != types.RunFailed {
		t.Fatalf("ordinary stale run after recovery = %s, want failed", gotRun.Status)
	}
	gotStep, err := fixture.database.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotStep.Status != types.StepStatusFailed {
		t.Fatalf("ordinary stale Push after recovery = %s, want failed", gotStep.Status)
	}
}
