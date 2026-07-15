package steps

import (
	"regexp"
	"strings"
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
	desc := strings.TrimSpace(item.Description)
	if desc == "" {
		return false
	}
	lower := strings.ToLower(desc)

	if claimsExternalOrNonOwnedLifecycle(lower) {
		return false
	}
	return claimsMissingPipelineOwnedDelivery(lower)
}

// externalPRRefPattern matches concrete external PR references that are not
// owned by this run's future PR step (numbered PRs, host URLs).
var externalPRRefPattern = regexp.MustCompile(`(?i)(?:\bpr\s*#\s*\d+\b|\bpull\s*request\s*#\s*\d+\b|\bpull/\d+\b|https?://[^\s]+/(?:pull|merge_requests)/\d+)`)

var deliverySurfacePattern = regexp.MustCompile(`\b(?:pr|prs|ci|push|pushed|pushes|pushing|check|checks)\b|\bpull requests?\b|\bremote branches?\b|\bon (?:a|the) remote\b|\bpipeline status\b`)

var deliveryClaimSeparatorPattern = regexp.MustCompile(`[.;]|\b(?:and|but|while|whereas|although)\b`)

func claimsExternalOrNonOwnedLifecycle(lower string) bool {
	if externalPRRefPattern.MatchString(lower) {
		return true
	}
	// Explicit external / pre-existing scope stays enforceable at review.
	for _, needle := range []string{
		"pre-existing",
		"preexisting",
		"already open",
		"already opened",
		"already exists",
		"already existing",
		"third-party",
		"third party",
		"external pr",
		"external pull request",
		"upstream pr",
		"upstream pull request",
		"must remain",
		"must stay",
		"keep open",
		"keep the pr",
		"keep pr",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	// Third-party / published artifact requirements are not this run's delivery.
	for _, needle := range []string{
		"third-party artifact",
		"third party artifact",
		"published artifact",
		"release artifact",
		"npm package",
		"pypi",
		"docker hub",
		"container registry",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func claimsMissingPipelineOwnedDelivery(lower string) bool {
	if !deliverySurfacePattern.MatchString(lower) {
		return false
	}

	// Must claim absence / not-yet for that delivery surface.
	for _, needle := range []string{
		"zero pr",
		"zero prs",
		"zero pull",
		"no pr",
		"no pull request",
		"no pull requests",
		"returned zero",
		"list returned zero",
		"still needs to be opened",
		"still need to be opened",
		"needs to be opened",
		"need to be opened",
		"has not been opened",
		"have not been opened",
		"has not been created",
		"have not been created",
		"has not been pushed",
		"have not been pushed",
		"not been pushed",
		"not yet pushed",
		"not yet opened",
		"not yet created",
		"not yet present",
		"does not exist",
		"do not exist",
		"doesn't exist",
		"don't exist",
		"not present on a remote",
		"not present on the remote",
		"not on a remote",
		"not on the remote",
		"missing pr",
		"missing pull request",
		"pr is missing",
		"pull request is missing",
		"no remote branch",
		"remote branch is missing",
		"branch does not exist on",
		"no ci",
		"ci has not",
		"checks have not",
		"no checks",
		"checks not",
		"not green",
		"ci not",
	} {
		if strings.Contains(lower, needle) {
			return onlyContainsDeliveryClaims(lower)
		}
	}

	// "PR A still needs to be opened" / "open a PR" as the defect claim.
	if strings.Contains(lower, "opened without merging") ||
		strings.Contains(lower, "still needs to be opened") ||
		(strings.Contains(lower, "open") && deliverySurfacePattern.MatchString(lower) &&
			(strings.Contains(lower, "still") || strings.Contains(lower, "not") || strings.Contains(lower, "zero") || strings.Contains(lower, "missing"))) {
		return onlyContainsDeliveryClaims(lower)
	}
	return false
}

func onlyContainsDeliveryClaims(lower string) bool {
	for _, fragment := range deliveryClaimSeparatorPattern.Split(lower, -1) {
		fragment = strings.TrimSpace(fragment)
		if fragment != "" && !deliverySurfacePattern.MatchString(fragment) {
			return false
		}
	}
	return true
}
