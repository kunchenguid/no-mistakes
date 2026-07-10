package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PushStep force-pushes the worktree state to the configured push remote.
type PushStep struct{}

func (s *PushStep) Name() types.StepName { return types.StepPush }

func (s *PushStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx

	// Push is transport-only: it never formats, stages, writes evidence, or
	// creates commits. It validates the sealed candidate - a clean worktree at
	// the exact sealed SHA - then transports it under the existing
	// force-with-lease and unseen-remote-commit protections.
	seal, err := sctx.DB.LatestSeal(sctx.Run.ID)
	if err != nil {
		return nil, fmt.Errorf("load sealed candidate: %w", err)
	}
	if seal == nil {
		return nil, fmt.Errorf("push: no sealed candidate to publish")
	}

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
	pushTarget := "upstream"
	usingFork := strings.TrimSpace(sctx.Repo.ForkURL) != ""
	if usingFork {
		pushTarget = "fork"
		sctx.Log(fmt.Sprintf("pushing to fork %s (%s)...", safeurl.Redact(pushURL), ref))
	} else {
		sctx.Log(fmt.Sprintf("pushing to %s (%s)...", safeurl.Redact(pushURL), ref))
	}

	// Decide whether force-pushing would discard commits the pipeline never saw.
	// The lease is anchored to the remote-tracking ref the rebase step freshly
	// fetched (the exact commit this branch was rebased against), so a push that
	// would clobber an out-of-band or stale-mirror commit fails loudly instead
	// of silently dropping it. A bare --force-with-lease offers no protection
	// when pushing to a URL (no remote-tracking refs), so the anchor is explicit.
	lastSeen := lastFetchedBranchTip(ctx, sctx.WorkDir, branch, usingFork)
	gitRun := func(args ...string) (string, error) { return git.Run(ctx, sctx.WorkDir, args...) }
	decision, err := resolveForcePushDecision(gitRun, pushURL, ref, headBeingPushed, lastSeen, sctx.Run.BaseSHA)
	if err != nil {
		return nil, fmt.Errorf("push to %s: %w", pushTarget, err)
	}
	switch {
	case decision.newBranch:
		// New branch: regular push (no force needed).
		if err := git.Push(ctx, sctx.WorkDir, pushURL, ref, "", false); err != nil {
			return nil, fmt.Errorf("push to %s: %w", pushTarget, err)
		}
	case decision.upToDate:
		// Remote already at this head: nothing to push, just reconcile refs below.
	default:
		// Existing branch: force-with-lease anchored to the verified remote head.
		if err := git.Push(ctx, sctx.WorkDir, pushURL, ref, decision.remoteSHA, true); err != nil {
			return nil, fmt.Errorf("push to %s: %w", pushTarget, err)
		}
	}

	if sctx.Run.HeadSHA != seal.SHA {
		sctx.Run.HeadSHA = seal.SHA
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, seal.SHA); err != nil {
			return nil, err
		}
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
