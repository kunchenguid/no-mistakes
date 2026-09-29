package steps

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A pending claim is retained for the owning evidence step. Review itself
// cannot make test, CI, or runtime observations authoritative.
const reviewSupportPendingCategory = "review-support-pending"

func validateReviewFindingSupport(sctx *pipeline.StepContext, findings Findings, reviewedHead string) (Findings, error) {
	if sctx == nil || sctx.PRContext == nil {
		// Direct fixture and eval calls have no pinned comparison. They retain
		// their existing behavior; a support object they do supply must still
		// have a valid shape.
		for i, item := range findings.Items {
			if item.Support != nil {
				if item.Support.OwnerResult != nil {
					return findings, fmt.Errorf("review finding %d cannot supply owner result", i)
				}
				if err := item.Support.Validate(); err != nil {
					return findings, fmt.Errorf("review finding %d support: %w", i, err)
				}
			}
		}
		return findings, nil
	}
	if reviewedHead == "" {
		return findings, fmt.Errorf("reviewed head is empty")
	}
	if reviewedHead != sctx.PRContext.LocalHeadSHA {
		// A Review fix can commit inside Execute before the executor's
		// post-step PR-context guard rebinds the receipt. Validate provisionally
		// against that exact new commit only when local HEAD and the run agree.
		// The post-step guard revokes this outcome and restarts Review if the
		// comparison changed; this function never mutates the receipt.
		if !sctx.Fixing || sctx.Run == nil || reviewedHead != sctx.Run.HeadSHA {
			return findings, fmt.Errorf("reviewed head %q differs from pinned PR comparison head %q", reviewedHead, sctx.PRContext.LocalHeadSHA)
		}
		actualHead, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
		if err != nil || actualHead != reviewedHead {
			return findings, fmt.Errorf("review fix head %q differs from local git HEAD %q: %v", reviewedHead, actualHead, err)
		}
	}
	for i := range findings.Items {
		item := &findings.Items[i]
		// Category is pipeline-owned. An agent cannot mark its own source
		// finding pending to bypass the Review blocker check.
		item.Category = ""
		if item.Support == nil {
			return findings, fmt.Errorf("review finding %d missing support", i)
		}
		if item.Support.OwnerResult != nil {
			return findings, fmt.Errorf("review finding %d cannot supply owner result", i)
		}
		if err := item.Support.Validate(); err != nil {
			return findings, fmt.Errorf("review finding %d support: %w", i, err)
		}
		switch item.Support.ClaimType {
		case types.FindingClaimSource:
			ref := item.Support.Source
			if ref.HeadSHA != "" && ref.HeadSHA != reviewedHead {
				markReviewSupportPending(item, false)
				continue
			}
			if err := validateSourceQuoteAtHead(sctx, reviewedHead, ref); err != nil {
				return findings, fmt.Errorf("review finding %d source support: %w", i, err)
			}
		case types.FindingClaimTest, types.FindingClaimCI:
			markReviewSupportPending(item, true)
		case types.FindingClaimRuntime:
			markReviewSupportPending(item, false)
		}
	}
	return findings, nil
}

func markReviewSupportPending(item *Finding, ownerStepWillCheck bool) {
	item.Category = reviewSupportPendingCategory
	if ownerStepWillCheck {
		// Review must advance to Test/CI. Its own approval/fix loop does not
		// own this claim, while the finding remains in the durable payload.
		item.Action = types.ActionNoOp
	} else {
		item.Action = types.ActionAskUser
	}
}

func hasBlockingReviewFindings(items []Finding) bool {
	for _, item := range items {
		if item.Category != reviewSupportPendingCategory || item.Action != types.ActionNoOp {
			if item.Severity == types.FindingSeverityError || item.Severity == types.FindingSeverityWarning {
				return true
			}
		}
	}
	return false
}

func validateSourceQuoteAtHead(sctx *pipeline.StepContext, head string, ref *types.FindingSourceSupport) error {
	if ref == nil || !filepath.IsLocal(ref.Path) || strings.ContainsAny(ref.Path, "\x00\r\n") {
		return fmt.Errorf("invalid source path")
	}
	// Git reads the object at the pinned commit. The working tree may have
	// moved while the reviewer was running, so reading a filesystem file here
	// would let an unreviewed revision substantiate the claim.
	content, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "show", head+":"+ref.Path)
	if err != nil {
		return fmt.Errorf("read %s at reviewed head: %w", ref.Path, err)
	}
	if bytes.IndexByte(content, 0) >= 0 {
		return fmt.Errorf("source path is not text")
	}
	lines := bytes.Split(content, []byte{'\n'})
	if ref.Line < 1 || ref.Line > len(lines) {
		return fmt.Errorf("source line %d does not exist", ref.Line)
	}
	line := strings.TrimSuffix(string(lines[ref.Line-1]), "\r")
	quote := strings.TrimSpace(ref.Quote)
	if strings.ContainsAny(quote, "\r\n") || !strings.Contains(line, quote) {
		return fmt.Errorf("source quote does not match line %d at reviewed head", ref.Line)
	}
	return nil
}
