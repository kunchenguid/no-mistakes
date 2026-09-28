package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// RecheckApprovalGate is intentionally separate from Execute and approval:
// it cannot call a repair agent, rerun jobs, publish, skip, or grant a waiver.
func (s *CIStep) RecheckApprovalGate(sctx *pipeline.StepContext, findings string) (string, error) {
	if !pipeline.CanRecheckCIProvider(findings) {
		return "", fmt.Errorf("only a provider-read CI gate can be rechecked; other findings still need a decision")
	}
	expected := strings.TrimSpace(sctx.Run.HeadSHA)
	if expected == "" {
		return "", fmt.Errorf("run has no recorded head")
	}
	if err := recheckLocalHead(sctx, expected); err != nil {
		return "", err
	}
	host, reason := buildHost(sctx, resolvedProvider(sctx))
	if host == nil {
		return "", fmt.Errorf("provider unavailable: %s", reason)
	}
	reader, ok := host.(scm.HeadCheckReader)
	if !ok {
		return "", fmt.Errorf("provider cannot verify exact-head CI rechecks")
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return "", err
	}
	if sctx.Run.PRURL == nil || strings.TrimSpace(*sctx.Run.PRURL) == "" {
		return "", fmt.Errorf("run has no PR URL")
	}
	number, err := scm.ExtractPRNumber(*sctx.Run.PRURL)
	if err != nil {
		return "", err
	}
	pr := &scm.PR{Number: number, URL: *sctx.Run.PRURL, HeadSHA: expected}
	state, err := host.GetPRState(sctx.Ctx, pr)
	if err != nil {
		return "", err
	}
	if state != scm.PRStateOpen {
		return "", fmt.Errorf("PR is not open: %s", state)
	}
	if host.Capabilities().MergeableState {
		mergeable, err := host.GetMergeableState(sctx.Ctx, pr)
		if err != nil {
			return "", err
		}
		if mergeable != scm.MergeableOK {
			return "", fmt.Errorf("PR mergeability is unresolved: %s", mergeable)
		}
	}
	checks, err := reader.GetChecksForHead(sctx.Ctx, pr, expected)
	if err != nil {
		return "", err
	}
	if !allChecksPassed(checks) {
		return "", fmt.Errorf("current-head checks are absent or not all passed")
	}
	if err := recheckLocalHead(sctx, expected); err != nil {
		return "", err
	}
	if err := sctx.Ctx.Err(); err != nil {
		return "", err
	}
	return fmt.Sprintf("Provider-only CI recheck verified %d checks at head %s; no override or repair", len(checks), expected), nil
}

func recheckLocalHead(sctx *pipeline.StepContext, expected string) error {
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return err
	}
	if head != expected {
		return fmt.Errorf("worktree head differs from the recorded CI head")
	}
	dirty, err := git.HasUncommittedChanges(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("worktree has uncommitted changes; a provider-only recheck cannot validate them")
	}
	return nil
}
