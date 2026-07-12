package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PushStep durably publishes the sealed worktree state to the configured push
// remote. transport is an acceptance-ambiguity seam used by focused tests.
type PushStep struct {
	transport publicationTransport
}

func (s *PushStep) Name() types.StepName { return types.StepPush }

func (s *PushStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	seal, err := sctx.DB.LatestSeal(sctx.Run.ID)
	if err != nil {
		return nil, fmt.Errorf("load sealed candidate: %w", err)
	}
	if seal == nil {
		return nil, fmt.Errorf("push: no sealed candidate to publish")
	}

	gitRun := func(args ...string) (string, error) { return git.Run(ctx, sctx.WorkDir, args...) }
	publication, err := sctx.DB.LatestPublication(sctx.Run.ID, db.PublicationKindPush)
	if err != nil {
		return nil, fmt.Errorf("load publication transaction: %w", err)
	}
	if publication != nil && publication.SealSHA != seal.SHA && publication.State != db.PublicationStateCompleted {
		return nil, fmt.Errorf("push: incomplete publication for sealed SHA %s blocks newer sealed SHA %s", shortSHA(publication.SealSHA), shortSHA(seal.SHA))
	}
	if publication == nil || publication.SealSHA != seal.SHA {
		status, err := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
		if err != nil {
			return nil, fmt.Errorf("read worktree status before push: %w", err)
		}
		if strings.TrimSpace(status) != "" {
			return nil, fmt.Errorf("push: refusing to publish a dirty worktree; sealed candidate %s must be published clean", shortSHA(seal.SHA))
		}
		headBeingPushed, err := git.HeadSHA(ctx, sctx.WorkDir)
		if err != nil {
			return nil, fmt.Errorf("resolve head before push: %w", err)
		}
		if headBeingPushed != seal.SHA {
			return nil, fmt.Errorf("push: HEAD %s no longer matches sealed candidate %s; a reverified candidate must be resealed before publishing", shortSHA(headBeingPushed), shortSHA(seal.SHA))
		}

		ref := normalizedBranchRef(sctx.Run.Branch)
		branch := strings.TrimPrefix(ref, "refs/heads/")
		pushURL := sctx.Repo.PushURL()
		usingFork := strings.TrimSpace(sctx.Repo.ForkURL) != ""
		lastSeen := lastFetchedBranchTip(ctx, sctx.WorkDir, branch, usingFork)
		decision, err := resolveForcePushDecision(gitRun, pushURL, ref, seal.SHA, lastSeen, sctx.Run.BaseSHA)
		if err != nil {
			return nil, fmt.Errorf("prepare publication: %w", err)
		}
		publication, err = sctx.DB.PreparePublication(db.PreparePublicationInput{
			RunID:             sctx.Run.ID,
			Kind:              db.PublicationKindPush,
			SealID:            seal.ID,
			SealSHA:           seal.SHA,
			DestinationURL:    pushURL,
			DestinationRef:    ref,
			ExpectedRemoteSHA: decision.remoteSHA,
			Force:             !decision.upToDate,
		})
		if err != nil {
			return nil, fmt.Errorf("prepare durable publication: %w", err)
		}
	}

	pushTarget := "upstream"
	if strings.TrimSpace(sctx.Repo.ForkURL) != "" {
		pushTarget = "fork"
	}
	sctx.Log(fmt.Sprintf("publishing sealed candidate to %s %s (%s)...", pushTarget, safeurl.Redact(publication.DestinationURL), publication.DestinationRef))
	if _, err := executePreparedPublication(sctx, publication, s.transport, gitRun, db.PublicationRecoveryNone, nil); err != nil {
		return nil, fmt.Errorf("push to %s: %w", pushTarget, err)
	}
	sctx.Log("pushed successfully")
	return &pipeline.StepOutcome{}, nil
}

func dirHasFiles(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if !d.IsDir() {
			found = true
		}
		return nil
	})
	return found
}
