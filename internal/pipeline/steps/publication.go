package steps

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// publicationContext loads destination changes at each publication boundary,
// including after a live approval gate. Only the executor writes its in-memory
// publication fields; daemon rebinding writes the DB under WhileParked. Keep
// the same run pointer so publication/CI head updates and lifecycle callbacks
// still reach the executor, and never rename its custody branch.
func publicationContext(sctx *pipeline.StepContext) (*pipeline.StepContext, error) {
	if sctx.DB == nil {
		return sctx, nil
	}
	fresh, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		return nil, fmt.Errorf("read publication destination: %w", err)
	}
	if fresh == nil || fresh.PublicationBranch == nil {
		return sctx, nil
	}
	repo, err := sctx.DB.GetRepo(sctx.Repo.ID)
	if err != nil || repo == nil || repo.UpstreamURL != sctx.Repo.UpstreamURL || repo.ForkURL != sctx.Repo.ForkURL || repo.DefaultBranch != sctx.Repo.DefaultBranch || repo.WorkingPath != sctx.Repo.WorkingPath {
		return nil, fmt.Errorf("rebound publication repository target changed; rerun against the intended target")
	}
	if *fresh.PublicationBranch == repo.DefaultBranch {
		return nil, fmt.Errorf("rebound publication cannot target the default branch")
	}
	if fresh.PublicationTargetFingerprint == nil || *fresh.PublicationTargetFingerprint != branchsync.TargetFingerprint(resolvePushURL(sctx)) {
		return nil, fmt.Errorf("rebound publication target changed; rebind against the intended configured target before continuing")
	}
	sctx.Run.PublicationBranch = fresh.PublicationBranch
	sctx.Run.PublicationTargetFingerprint = fresh.PublicationTargetFingerprint
	sctx.Run.PRURL = fresh.PRURL
	// Publication is bound to the registered upstream, even if a local
	// origin was independently repointed. Keep the run pointer unchanged.
	scopedCtx, scopedRepo := *sctx, *sctx.Repo
	scopedRepo.URLsVerified = true
	scopedCtx.Repo = &scopedRepo
	return &scopedCtx, nil
}

func assertReboundPublicationPR(sctx *pipeline.StepContext, remoteHead string) error {
	if sctx.Run.PublicationBranch == nil {
		return nil
	}
	host, reason := PublicationHost(sctx)
	if host == nil {
		return fmt.Errorf("rebound publication provider unavailable: %s", reason)
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return fmt.Errorf("rebound publication PR cannot be verified: %w", err)
	}
	pr, err := host.FindPR(sctx.Ctx, sctx.Run.PublishBranch(), "")
	if err != nil || pr == nil || sctx.Run.PRURL == nil || pr.URL != *sctx.Run.PRURL {
		return fmt.Errorf("rebound publication requires the same existing open PR; creation or replacement is refused")
	}
	reader, ok := host.(scm.PRHeadReader)
	if !ok {
		return fmt.Errorf("rebound publication provider cannot prove the PR head")
	}
	pr.HeadSHA, err = reader.GetPRHeadSHA(sctx.Ctx, pr, sctx.Run.PublishBranch())
	if err != nil || pr.HeadSHA == "" || pr.HeadSHA != remoteHead {
		return fmt.Errorf("rebound publication PR head changed")
	}
	state, err := host.GetPRState(sctx.Ctx, pr)
	if err != nil || state != scm.PRStateOpen {
		return fmt.Errorf("rebound publication PR is retired or unverifiable")
	}
	return nil
}

func assertReboundCIHead(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) error {
	if sctx.Run.PublicationBranch == nil {
		return nil
	}
	if pr.HeadSHA != "" && pr.HeadSHA != sctx.Run.HeadSHA {
		return fmt.Errorf("rebound CI checks describe head %s; run head is %s", pr.HeadSHA, sctx.Run.HeadSHA)
	}
	reader, ok := host.(scm.PRHeadReader)
	if !ok {
		return fmt.Errorf("rebound CI provider cannot prove the publication head")
	}
	head, err := reader.GetPRHeadSHA(sctx.Ctx, pr, sctx.Run.PublishBranch())
	if err != nil {
		return fmt.Errorf("rebound CI publication head is unverifiable: %w", err)
	}
	pr.HeadSHA = head
	if head != sctx.Run.HeadSHA {
		return fmt.Errorf("rebound CI destination has head %s; run head %s is unpublished there", head, sctx.Run.HeadSHA)
	}
	return nil
}
