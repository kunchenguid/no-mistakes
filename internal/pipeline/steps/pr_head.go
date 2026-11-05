package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// verifyPublishedPR is the shared publication postcondition. Read the durable
// successful-push binding, never the executor's potentially stale in-memory run
// or mutable Git HEAD. Refusal leaves that binding and custody evidence intact.
func verifyPublishedPR(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) error {
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		return fmt.Errorf("verify published PR: read successful push binding: %w", err)
	}
	if run == nil || run.RepoID != sctx.Repo.ID || run.LastPushedSHA == nil || strings.TrimSpace(*run.LastPushedSHA) == "" || run.PushActive || run.PushTargetFingerprint == nil || run.PushTargetKind == nil || run.PushRef == nil || run.PushGeneration == nil || *run.PushGeneration < 1 {
		return fmt.Errorf("verify published PR: complete successful push binding is unavailable")
	}
	kind := "upstream"
	if strings.TrimSpace(sctx.Repo.ForkURL) != "" {
		kind = "fork"
	}
	if *run.PushTargetKind != kind || *run.PushTargetFingerprint != branchsync.TargetFingerprint(resolvePushURL(sctx)) || *run.PushRef != normalizedBranchRef(run.Branch) || normalizedBranchRef(run.Branch) != normalizedBranchRef(sctx.Run.Branch) {
		return fmt.Errorf("verify published PR: successful push binding does not match this run's repository target and branch")
	}
	verifier, ok := host.(scm.PRHeadVerifier)
	if !ok {
		return fmt.Errorf("verify published PR: provider cannot prove the live PR identity and exact pushed head: %w", scm.ErrUnsupported)
	}
	if err := verifier.VerifyPRHead(sctx.Ctx, pr, strings.TrimPrefix(*run.PushRef, "refs/heads/"), *run.LastPushedSHA); err != nil {
		return fmt.Errorf("verify published PR: %w", err)
	}
	return nil
}
