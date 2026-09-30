package steps

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var errReviewSupportHeadAdvanced = errors.New("owner repair advanced head; review must restart")

// currentReviewSupportClaims reads only the completed Review step attached to
// this run. No round history, previous finding, or model output at the owner
// step grants authority. A pending claim can be examined only with an
// approved Review and the owner's current PR comparison.
func currentReviewSupportClaims(sctx *pipeline.StepContext, claimType types.FindingClaimType) ([]Finding, *db.PRContext, error) {
	if sctx == nil || sctx.DB == nil || sctx.Run == nil {
		return nil, nil, fmt.Errorf("review support owner needs run and database")
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return nil, nil, err
	}
	var review *db.StepResult
	for _, step := range steps {
		if step.StepName != types.StepReview || step.Status != types.StepStatusCompleted {
			continue
		}
		if review != nil {
			return nil, nil, fmt.Errorf("multiple completed Review steps for run")
		}
		review = step
	}
	if review == nil || review.FindingsJSON == nil {
		return nil, nil, nil
	}
	findings, err := types.ParseFindingsJSON(*review.FindingsJSON)
	if err != nil {
		return nil, nil, fmt.Errorf("read completed Review findings: %w", err)
	}
	var pending []Finding
	for _, item := range findings.Items {
		if item.Category != reviewSupportPendingCategory || item.Support == nil || item.Support.ClaimType != claimType {
			continue
		}
		if item.ID == "" || item.Support.OwnerResult != nil {
			return nil, nil, fmt.Errorf("completed Review has invalid pending support identity")
		}
		if err := item.Support.Validate(); err != nil {
			return nil, nil, fmt.Errorf("completed Review pending support: %w", err)
		}
		pending = append(pending, item)
	}
	if len(pending) == 0 {
		return nil, nil, nil
	}
	receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil {
		return nil, nil, err
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		return nil, nil, err
	}
	if receipt != nil && run != nil && sctx.PRContext != nil &&
		sctx.Fixing && receipt.LocalHeadSHA != sctx.Run.HeadSHA && run.HeadSHA == sctx.Run.HeadSHA {
		actualHead, headErr := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
		if headErr == nil && actualHead == sctx.Run.HeadSHA {
			return nil, nil, errReviewSupportHeadAdvanced
		}
	}
	if receipt == nil || run == nil || run.ReviewApprovedHeadSHA == nil ||
		strings.TrimSpace(*run.ReviewApprovedHeadSHA) == "" ||
		run.HeadSHA != receipt.LocalHeadSHA || sctx.Run.HeadSHA != receipt.LocalHeadSHA ||
		sctx.PRContext == nil || *sctx.PRContext != *receipt {
		return nil, nil, fmt.Errorf("pending Review support has no exact current PR comparison")
	}
	return pending, receipt, nil
}

// ownerSupportFinding records a trusted owner observation with a stable key.
// Unresolved and supported records block the owner step. Only a current
// disproving observation is informational.
func ownerSupportFinding(claim Finding, receipt *db.PRContext, disposition, description, observedAt string, exitCode *int, checkState string) Finding {
	support := *claim.Support
	support.OwnerResult = &types.FindingOwnerResult{
		ReviewFindingID: claim.ID,
		HeadSHA:         receipt.LocalHeadSHA,
		TargetSHA:       receipt.TargetSHA,
		DiffDigest:      receipt.DiffDigest,
		Generation:      receipt.Generation,
		ObservedAt:      observedAt,
		Disposition:     disposition,
		ExitCode:        exitCode,
		CheckState:      checkState,
	}
	result := Finding{
		ID:          types.ReviewSupportClaimID(claim),
		Severity:    types.FindingSeverityInfo,
		Action:      types.ActionNoOp,
		Category:    types.FindingCategoryReviewSupportResolved,
		Description: description,
		Support:     &support,
	}
	if disposition == types.FindingSupportDispositionSupported {
		result.Severity = types.FindingSeverityError
		result.Action = types.ActionAskUser
	}
	if disposition == types.FindingSupportDispositionUnresolved {
		result.Severity = types.FindingSeverityError
		result.Action = types.ActionAskUser
		result.Category = types.FindingCategoryReviewSupportUnresolved
	}
	return result
}

func supportObservationTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// resolveTestReviewSupport uses only the configured command this Test step
// already ran. A command string in Review output is a selector, never a shell
// instruction. Agent-reported tests do not satisfy this owner result.
func resolveTestReviewSupport(sctx *pipeline.StepContext, configuredCommand string, exitCode *int, commandHead, observedAt string) ([]Finding, error) {
	claims, receipt, err := currentReviewSupportClaims(sctx, types.FindingClaimTest)
	if err != nil || len(claims) == 0 {
		return nil, err
	}
	if observedAt == "" {
		observedAt = supportObservationTime()
	}
	results := make([]Finding, 0, len(claims))
	for _, claim := range claims {
		if configuredCommand == "" || claim.Support.Test.Command != configuredCommand || exitCode == nil || commandHead != receipt.LocalHeadSHA {
			results = append(results, ownerSupportFinding(claim, receipt, types.FindingSupportDispositionUnresolved,
				"Review test claim has no matching current configured command result", observedAt, nil, ""))
			continue
		}
		disposition := types.FindingSupportDispositionDisproven
		description := "Review test claim disproven by current configured command"
		if *exitCode != 0 {
			disposition = types.FindingSupportDispositionSupported
			description = "Review test claim supported by current configured command failure"
		}
		results = append(results, ownerSupportFinding(claim, receipt, disposition, description, observedAt, exitCode, ""))
	}
	return results, nil
}

func currentTestCommandHead(sctx *pipeline.StepContext) string {
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return ""
	}
	return head
}

// resolveCIReviewSupport makes a fresh provider read for the exact current PR
// head and comparison. The named immutable provider check must belong to
// that head; unrelated passing checks cannot resolve a historical claim.
func resolveCIReviewSupport(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) ([]Finding, error) {
	claims, receipt, err := currentReviewSupportClaims(sctx, types.FindingClaimCI)
	if err != nil || len(claims) == 0 {
		return nil, err
	}
	observedAt := supportObservationTime()
	unresolved := func(reason string) []Finding {
		results := make([]Finding, 0, len(claims))
		for _, claim := range claims {
			results = append(results, ownerSupportFinding(claim, receipt, types.FindingSupportDispositionUnresolved, reason, observedAt, nil, ""))
		}
		return results
	}
	if host == nil || pr == nil || receipt.PRURL == "" {
		return unresolved("Review CI claim has no current PR check source"), nil
	}
	reader, ok := host.(scm.PRFactsReader)
	if !ok {
		return unresolved("Provider cannot read current PR facts for Review CI claim"), nil
	}
	facts, err := reader.ReadPRFacts(sctx.Ctx, pr)
	if err != nil || (facts.State != scm.PRStateOpen && facts.State != scm.PRStateMerged && facts.State != scm.PRStateClosed) || !scm.SameSourceRepository(resolvedProvider(sctx), facts.SourceRepository, receipt.SourceRepo) ||
		facts.SourceBranch != receipt.SourceBranch || facts.PR.URL != receipt.PRURL ||
		facts.HeadSHA != receipt.LocalHeadSHA || facts.BaseBranch != receipt.TargetBranch {
		return unresolved("Review CI claim has no exact current PR head observation"), nil
	}
	selection := pipeline.PRTargetSelection{PRURL: facts.PR.URL, State: facts.State, SourceRepo: facts.SourceRepository,
		SourceBranch: facts.SourceBranch, ForgeHeadSHA: facts.HeadSHA, TargetBranch: facts.BaseBranch}
	if err := verifyCurrentPRComparison(sctx, host, selection, receipt, &facts); err != nil {
		return unresolved("Review CI claim has no exact current PR comparison: " + err.Error()), nil
	}
	checkPR := *pr
	checkPR.HeadSHA = receipt.LocalHeadSHA
	checks, err := host.GetChecks(sctx.Ctx, &checkPR)
	if err != nil {
		return unresolved("Review CI claim check read failed"), nil
	}
	observedAt = supportObservationTime()
	results := make([]Finding, 0, len(claims))
	for _, claim := range claims {
		ref := claim.Support.CI
		if ref.HeadSHA != receipt.LocalHeadSHA {
			results = append(results, ownerSupportFinding(claim, receipt, types.FindingSupportDispositionUnresolved,
				"Historical Review CI claim has no matching named check evidence on the current head", observedAt, nil, ""))
			continue
		}
		var matching *scm.Check
		ambiguous := false
		for i := range checks {
			if checks[i].ProviderID != ref.CheckID || checks[i].HeadSHA != receipt.LocalHeadSHA {
				continue
			}
			if matching != nil {
				ambiguous = true
				break
			}
			matching = &checks[i]
		}
		if ambiguous || matching == nil {
			results = append(results, ownerSupportFinding(claim, receipt, types.FindingSupportDispositionUnresolved,
				"Review CI check identity has no unique current-head match", observedAt, nil, ""))
			continue
		}
		checkState := string(matching.Bucket)
		if matching.State != "" {
			checkState += ":" + matching.State
		}
		if matching.PreRunFailure {
			results = append(results, ownerSupportFinding(claim, receipt, types.FindingSupportDispositionUnresolved,
				"Review CI check failed before repository code ran", observedAt, nil, checkState))
			continue
		}
		switch matching.Bucket {
		case scm.CheckBucketPass:
			results = append(results, ownerSupportFinding(claim, receipt, types.FindingSupportDispositionDisproven,
				"Review CI claim disproven by current-head check", observedAt, nil, checkState))
		case scm.CheckBucketFail:
			results = append(results, ownerSupportFinding(claim, receipt, types.FindingSupportDispositionSupported,
				"Review CI claim supported by current-head failed check", observedAt, nil, checkState))
		default:
			results = append(results, ownerSupportFinding(claim, receipt, types.FindingSupportDispositionUnresolved,
				"Review CI check has no terminal pass or failure", observedAt, nil, checkState))
		}
	}
	return results, nil
}

func appendOwnerSupportResults(outcome *pipeline.StepOutcome, results []Finding) error {
	if outcome == nil || len(results) == 0 {
		return nil
	}
	var findings Findings
	if outcome.Findings != "" {
		parsed, err := types.ParseFindingsJSON(outcome.Findings)
		if err != nil {
			return fmt.Errorf("read owner step findings: %w", err)
		}
		findings = parsed
	}
	findings.Items = append(findings.Items, results...)
	for _, result := range results {
		if result.Severity == types.FindingSeverityError || result.Severity == types.FindingSeverityWarning {
			outcome.NeedsApproval = true
		}
	}
	encoded, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return err
	}
	outcome.Findings = encoded
	return nil
}
