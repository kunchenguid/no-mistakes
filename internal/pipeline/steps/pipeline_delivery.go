package steps

import (
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// pipelineDeliveryPhaseClause documents the pre-push ownership boundary for
// the review step. Review runs before push, PR, and CI (types.StepName.Order).
// Those later steps own remote-branch, pull-request, and CI outcomes for this
// run, so source review must not treat their absence as a defect.
func pipelineDeliveryPhaseClause() string {
	return "\n\nPipeline phase (review is pre-push): this same run owns push, pull-request creation or update, and CI monitoring in later pipeline steps. Do NOT emit findings solely because the remote branch, push, pull request, or CI for this run's change is missing or not yet present - those are outputs this pipeline produces later. Continue reviewing the implementation and every source-verifiable acceptance criterion. Requirements about a pre-existing external PR, a specific third-party artifact, or lifecycle state not owned by the current run remain fully enforceable."
}

// stripDeferredPipelineOwnedDeliveryFindings removes review findings that only
// assert a later pipeline-owned delivery outcome has not happened yet. Review
// is always pre-push, so such findings are phase-invalid. External or already-
// required lifecycle state is left alone.
//
// Returns the filtered findings and how many items were dropped.
func stripDeferredPipelineOwnedDeliveryFindings(findings Findings) (Findings, int) {
	if len(findings.Items) == 0 {
		return findings, 0
	}
	kept := make([]Finding, 0, len(findings.Items))
	dropped := 0
	for _, item := range findings.Items {
		if isDeferredPipelineOwnedDeliveryFinding(item) {
			dropped++
			continue
		}
		kept = append(kept, item)
	}
	if dropped == 0 {
		return findings, 0
	}
	out := findings
	out.Items = kept
	if len(kept) == 0 && strings.TrimSpace(out.Summary) != "" {
		// Keep a truthful summary when the only findings were deferred.
		out.Summary = "no source-review findings (deferred pipeline-owned delivery claims dropped)"
	}
	return out, dropped
}

// isDeferredPipelineOwnedDeliveryFinding reports whether a finding's claim is
// only that this run's later-owned delivery artifacts (remote branch, push,
// PR open/update, CI) are not yet present. That class is invalid at pre-push
// review. Findings about pre-existing external PRs, third-party artifacts, or
// other non-run-owned lifecycle state return false.
func isDeferredPipelineOwnedDeliveryFinding(item Finding) bool {
	return item.ReviewScope == types.FindingReviewScopePipelineOwnedDelivery
}
