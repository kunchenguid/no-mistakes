package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

type publicationTransport func(context.Context, string, string, string, string, string, bool) error
type publicationCleanup func(*pipeline.StepContext, *db.Publication) error

func defaultPublicationTransport(ctx context.Context, workDir, destinationURL, sourceSHA, destinationRef, expectedRemoteSHA string, force bool) error {
	return git.Push(ctx, workDir, destinationURL, sourceSHA, destinationRef, expectedRemoteSHA, force)
}

func executePreparedPublication(
	sctx *pipeline.StepContext,
	publication *db.Publication,
	transport publicationTransport,
	gitRun gitRunner,
	recoveryMode db.PublicationRecoveryStepMode,
	cleanup publicationCleanup,
) (bool, error) {
	if publication == nil {
		return false, fmt.Errorf("execute publication: missing durable transaction")
	}
	if transport == nil {
		transport = defaultPublicationTransport
	}

	transported := false
	state := publication.State
	initialState := state
	if state == db.PublicationStatePrepared {
		if err := sctx.DB.MarkPublicationAttempted(publication.ID); err != nil {
			return false, fmt.Errorf("mark publication attempted: %w", err)
		}
		state = db.PublicationStateAttempted
	}
	if state == db.PublicationStateAttempted {
		remoteSHA, err := lsRemoteSHA(gitRun, publication.DestinationURL, publication.DestinationRef)
		if err != nil {
			return false, fmt.Errorf("reconcile publication destination: %w", err)
		}
		if remoteSHA == publication.SealSHA {
			if err := sctx.DB.MarkPublicationAccepted(publication.ID); err != nil {
				return false, fmt.Errorf("record accepted publication: %w", err)
			}
			state = db.PublicationStateAccepted
		} else {
			if remoteSHA != publication.ExpectedRemoteSHA {
				return false, fmt.Errorf(
					"publication destination %s moved from expected %s to %s; refusing to republish sealed SHA %s",
					publication.DestinationRef,
					shortPublicationSHA(publication.ExpectedRemoteSHA),
					shortPublicationSHA(remoteSHA),
					shortSHA(publication.SealSHA),
				)
			}
			pushErr := transport(
				sctx.Ctx,
				sctx.WorkDir,
				publication.DestinationURL,
				publication.SealSHA,
				publication.DestinationRef,
				publication.ExpectedRemoteSHA,
				publication.Force,
			)
			if pushErr != nil {
				acceptedSHA, reconcileErr := lsRemoteSHA(gitRun, publication.DestinationURL, publication.DestinationRef)
				if reconcileErr != nil {
					return false, errors.Join(fmt.Errorf("transport sealed publication: %w", pushErr), fmt.Errorf("reconcile ambiguous publication: %w", reconcileErr))
				}
				if acceptedSHA != publication.SealSHA {
					return false, fmt.Errorf("transport sealed publication: %w", pushErr)
				}
			} else {
				transported = true
			}
			if err := sctx.DB.MarkPublicationAccepted(publication.ID); err != nil {
				return false, fmt.Errorf("record accepted publication: %w", err)
			}
			state = db.PublicationStateAccepted
		}
	}
	if state == db.PublicationStateAccepted {
		if _, err := stepGitRun(sctx, "update-ref", publication.DestinationRef, publication.SealSHA); err != nil {
			return transported, fmt.Errorf("reconcile local publication ref: %w", err)
		}
		if cleanup == nil {
			cleanup = defaultPublicationCleanup
		}
		if err := cleanup(sctx, publication); err != nil {
			return transported, fmt.Errorf("restore local state after accepted publication: %w", err)
		}
		if err := sctx.DB.CompletePublication(publication.ID, recoveryMode); err != nil {
			return transported, fmt.Errorf("complete publication: %w", err)
		}
		state = db.PublicationStateCompleted
		if err := removeCompletedPublicationSnapshot(sctx, publication); err != nil {
			return transported, err
		}
	}
	if state != db.PublicationStateCompleted {
		return transported, fmt.Errorf("publication %s has unsupported state %q", publication.ID, state)
	}
	if initialState == db.PublicationStateCompleted {
		if publication.CleanupSnapshotDir == "" {
			if _, err := stepGitRun(sctx, "update-ref", publication.DestinationRef, publication.SealSHA); err != nil {
				return transported, fmt.Errorf("reconcile local publication ref: %w", err)
			}
			if err := reconcilePublicationWorktree(sctx, publication.SealSHA); err != nil {
				return transported, err
			}
		} else if err := removeCompletedPublicationSnapshot(sctx, publication); err != nil {
			return transported, err
		}
		if recoveryMode != db.PublicationRecoveryNone {
			if err := sctx.DB.CompletePublication(publication.ID, recoveryMode); err != nil {
				return transported, fmt.Errorf("complete recovered publication: %w", err)
			}
		}
	}
	sctx.Run.HeadSHA = publication.SealSHA
	return transported, nil
}

func defaultPublicationCleanup(sctx *pipeline.StepContext, publication *db.Publication) error {
	if publication.CleanupSnapshotDir == "" {
		return reconcilePublicationWorktree(sctx, publication.SealSHA)
	}
	snapshot, err := loadCIPublicationSnapshot(sctx, publication.CleanupSnapshotDir)
	if err != nil {
		return err
	}
	return snapshot.restoreFilesystemAtSealedSHA(sctx, publication.SealSHA)
}

func removeCompletedPublicationSnapshot(sctx *pipeline.StepContext, publication *db.Publication) error {
	if publication.CleanupSnapshotDir == "" {
		return nil
	}
	if _, err := os.Lstat(publication.CleanupSnapshotDir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect completed publication cleanup snapshot: %w", err)
	}
	rootDir, err := validateCIPublicationSnapshotRoot(sctx, publication.CleanupSnapshotDir)
	if err != nil {
		return fmt.Errorf("validate completed publication cleanup snapshot: %w", err)
	}
	if err := os.RemoveAll(rootDir); err != nil {
		return fmt.Errorf("remove completed publication cleanup snapshot: %w", err)
	}
	return nil
}

func reconcilePublicationWorktree(sctx *pipeline.StepContext, sha string) error {
	if _, err := stepGitRun(sctx, "reset", "--hard", sha); err != nil {
		return fmt.Errorf("reset worktree to sealed publication %s: %w", shortSHA(sha), err)
	}
	if _, err := stepGitRun(sctx, "clean", "-fd"); err != nil {
		return fmt.Errorf("remove unsealed worktree paths: %w", err)
	}
	head, err := stepGitHeadSHA(sctx)
	if err != nil {
		return fmt.Errorf("validate publication HEAD: %w", err)
	}
	if head != sha {
		return fmt.Errorf("publication HEAD is %s, want sealed SHA %s", shortSHA(head), shortSHA(sha))
	}
	status, err := stepGitRun(sctx, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("validate publication worktree: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("publication worktree is not clean at sealed SHA %s", shortSHA(sha))
	}
	indexTree, err := stepGitRun(sctx, "write-tree")
	if err != nil {
		return fmt.Errorf("validate publication index: %w", err)
	}
	sealedTree, err := stepGitRun(sctx, "rev-parse", sha+"^{tree}")
	if err != nil {
		return fmt.Errorf("resolve sealed publication tree: %w", err)
	}
	if indexTree != sealedTree {
		return fmt.Errorf("publication index tree %s does not match sealed tree %s", shortSHA(indexTree), shortSHA(sealedTree))
	}
	return nil
}

// RecoverPublication replays or reconciles one durable transaction using only
// its immutable destination, ref, lease, and sealed SHA. It is safe to call for
// both incomplete transport and a completed transport whose executor step did
// not reach its terminal projection before a daemon crash.
func RecoverPublication(ctx context.Context, database *db.DB, publication *db.Publication, run *db.Run, repo *db.Repo, workDir string) error {
	if publication == nil || run == nil || repo == nil {
		return fmt.Errorf("recover publication: incomplete recovery inputs")
	}
	if publication.RunID != run.ID {
		return fmt.Errorf("recover publication: transaction run %s does not match run %s", publication.RunID, run.ID)
	}
	sctx := &pipeline.StepContext{
		Ctx:      ctx,
		Run:      run,
		Repo:     repo,
		WorkDir:  workDir,
		DB:       database,
		Log:      func(string) {},
		LogChunk: func(string) {},
		LogFile:  func(string) {},
	}
	gitRun := func(args ...string) (string, error) {
		return git.Run(ctx, workDir, args...)
	}
	mode := db.PublicationRecoveryCompletePush
	switch publication.Kind {
	case db.PublicationKindPush:
	case db.PublicationKindCI:
		mode = db.PublicationRecoveryResetCI
	default:
		return fmt.Errorf("recover publication: unsupported kind %q", publication.Kind)
	}
	if _, err := executePreparedPublication(sctx, publication, defaultPublicationTransport, gitRun, mode, nil); err != nil {
		return err
	}
	if publication.Kind == db.PublicationKindCI {
		if err := clearCIRepublishPending(sctx); err != nil {
			return err
		}
	}
	return nil
}

// RecoverLegacyCIPublication upgrades a pre-journal CI seal or protected ref
// into the durable transaction and then projects the interrupted CI step back
// to pending. New publications never need this path because seal and journal
// preparation are atomic.
func RecoverLegacyCIPublication(ctx context.Context, database *db.DB, run *db.Run, repo *db.Repo, workDir string) (bool, error) {
	if run == nil || repo == nil {
		return false, fmt.Errorf("recover legacy CI publication: incomplete recovery inputs")
	}
	sctx := &pipeline.StepContext{
		Ctx:      ctx,
		Run:      run,
		Repo:     repo,
		WorkDir:  workDir,
		DB:       database,
		Log:      func(string) {},
		LogChunk: func(string) {},
		LogFile:  func(string) {},
	}
	existing, err := database.LatestPublication(run.ID, db.PublicationKindCI)
	if err != nil {
		return false, err
	}
	if existing != nil {
		return false, nil
	}
	sha, err := pendingCIRepublishSHA(sctx)
	if err != nil {
		return true, err
	}
	if sha == "" {
		seal, err := database.LatestSealByReason(run.ID, "ci_republish")
		if err != nil {
			return true, err
		}
		if seal == nil {
			return false, nil
		}
		sha = seal.SHA
	}
	if _, err := (&CIStep{}).pushUpdatedHeadSHA(sctx, sha); err != nil {
		return true, err
	}
	publication, err := database.LatestPublication(run.ID, db.PublicationKindCI)
	if err != nil {
		return true, err
	}
	if publication == nil || publication.State != db.PublicationStateCompleted {
		return true, fmt.Errorf("recover legacy CI publication: durable transaction did not complete")
	}
	if err := database.CompletePublication(publication.ID, db.PublicationRecoveryResetCI); err != nil {
		return true, err
	}
	return true, nil
}

func shortPublicationSHA(sha string) string {
	if strings.TrimSpace(sha) == "" {
		return "<absent>"
	}
	return shortSHA(sha)
}
