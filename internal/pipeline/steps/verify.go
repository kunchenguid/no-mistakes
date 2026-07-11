package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// VerifyStep gates the sealed release candidate. It skips only when the sealed
// candidate exactly matches the latest strong-reviewed candidate, and otherwise
// performs a fresh aggregate verification of the later diff plus deterministic
// evidence. Blocking or inconclusive findings gate the pipeline and route
// through the common repair coordinator before Push.
type VerifyStep struct{}

func (s *VerifyStep) Name() types.StepName { return types.StepVerify }

func (s *VerifyStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx

	// The candidate was sealed after the last pre-Verify content mutator (Lint).
	// Verify certifies that exact SHA.
	seal, err := sctx.DB.LatestSeal(sctx.Run.ID)
	if err != nil {
		return nil, fmt.Errorf("verify: load sealed candidate: %w", err)
	}
	if seal == nil {
		return nil, fmt.Errorf("verify: no sealed candidate to verify")
	}

	// Skip only when the sealed candidate exactly matches the latest successful
	// strong-reviewed candidate - i.e. nothing changed since the strong review.
	reviewed, err := sctx.DB.LatestSealByReason(sctx.Run.ID, "reviewed")
	if err != nil {
		return nil, fmt.Errorf("verify: load reviewed candidate: %w", err)
	}
	if reviewed != nil && reviewed.SHA == seal.SHA {
		sctx.Log("verify: sealed candidate unchanged since strong review, skipping fresh verification")
		return &pipeline.StepOutcome{}, nil
	}

	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	purpose := verifyPurpose(sctx)
	sctx.Log(fmt.Sprintf("verifying candidate %s (%s)...", shortSHA(seal.SHA), purpose))

	candidateSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("verify: resolve candidate HEAD: %w", err)
	}
	if candidateSHA != seal.SHA {
		return nil, fmt.Errorf("verify: candidate HEAD %s does not match sealed candidate %s", shortSHA(candidateSHA), shortSHA(seal.SHA))
	}

	result, err := sctx.InvokeAgent(purpose, agent.RunOpts{
		Prompt:     buildVerifyPrompt(sctx, baseSHA, seal, reviewed),
		CWD:        sctx.WorkDir,
		JSONSchema: reviewFindingsSchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		return nil, fmt.Errorf("verify: aggregate verification: %w", err)
	}
	if err := requireUnchangedCleanCandidate(sctx, candidateSHA); err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	// A schema-incomplete verdict is inconclusive, which must block rather than
	// pass as verified even when an adapter claimed schema conformance.
	if result == nil {
		return nil, fmt.Errorf("verify: aggregate verification returned no result")
	}
	findings, err := validateReviewFindingsOutput(result.Output)
	if err != nil {
		return nil, fmt.Errorf("verify: inconclusive verification output: %w", err)
	}

	needsApproval := hasBlockingFindings(findings.Items)
	findingsJSON, _ := json.Marshal(findings)

	// A clean verification makes this exact unchanged SHA the latest
	// strong-reviewed candidate, so a later identical candidate can skip
	// re-verification.
	if !needsApproval {
		if err := sealReviewedCandidate(sctx, candidateSHA); err != nil {
			return nil, fmt.Errorf("verify: %w", err)
		}
	}

	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   len(findings.Items) > 0,
		Findings:      string(findingsJSON),
	}, nil
}

// verifyPurpose selects authority verification whenever transient intent or
// immutable run history says the candidate crossed a higher-risk boundary.
func verifyPurpose(sctx *pipeline.StepContext) types.Purpose {
	if strings.TrimSpace(sctx.UserIntent) != "" || sctx.Fixing || durableHistoryRequiresAuthority(sctx) {
		return types.PurposeEscalatedAggregateVerification
	}
	return types.PurposeNormalAggregateVerification
}

func durableHistoryRequiresAuthority(sctx *pipeline.StepContext) bool {
	attempts, err := sctx.DB.GetInvocationAttemptsByRun(sctx.Run.ID)
	if err != nil {
		return true
	}
	attemptByID := make(map[string]*db.InvocationAttempt, len(attempts))
	for _, attempt := range attempts {
		attemptByID[attempt.ID] = attempt
		if attempt.Start.Candidate.Profile == string(config.ProfileAuthorityStrong) {
			return true
		}
	}

	repairs, err := sctx.DB.GetFindingRepairsByRun(sctx.Run.ID)
	if err != nil {
		return true
	}
	for _, repair := range repairs {
		if repair.Verdict == db.RepairVerdictInconclusive || repair.Status == db.RepairStatusPending || repair.Status == db.RepairStatusFailed {
			return true
		}
		if repair.Severity != "error" && repair.Severity != "warning" {
			continue
		}
		fixer := attemptByID[repair.FixerAttemptID]
		if repair.Status != db.RepairStatusResolved && (fixer == nil || fixer.Start.Candidate.Profile == string(config.ProfileFixBalanced)) {
			return true
		}
	}
	return initialReviewRequiresAuthority(sctx)
}

// initialReviewRequiresAuthority reads the immutable first completed review
// round, not the mutable StepResult projection overwritten by later rounds.
func initialReviewRequiresAuthority(sctx *pipeline.StepContext) bool {
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return true
	}
	for _, sr := range steps {
		if sr.StepName != types.StepReview {
			continue
		}
		rounds, err := sctx.DB.GetRoundsByStep(sr.ID)
		if err != nil {
			return true
		}
		for _, round := range rounds {
			if round.Round != 1 {
				continue
			}
			if round.FindingsJSON == nil {
				return true
			}
			findings, err := types.ParseFindingsJSON(*round.FindingsJSON)
			if err != nil {
				return true
			}
			return strings.EqualFold(strings.TrimSpace(findings.RiskLevel), "high")
		}
		return true
	}
	return false
}

func buildVerifyPrompt(sctx *pipeline.StepContext, baseSHA string, seal, reviewed *db.Seal) string {
	reviewedScope := "the initial strong review"
	if reviewed != nil {
		reviewedScope = fmt.Sprintf("the last strong-reviewed candidate %s", shortSHA(reviewed.SHA))
	}
	prompt := fmt.Sprintf(
		`You are performing the final aggregate verification of a sealed release candidate before it is published.

Candidate: %s
Base commit: %s

Since %s, the candidate accumulated further changes (for example test fixes, documentation, formatting, lint fixes, or conflict resolutions). Review the WHOLE candidate diff against the base commit, paying particular attention to the changes made after the initial review.

Rules:
- Confirm the later changes introduced no correctness, security, or data-loss regressions.
- Confirm the candidate is internally coherent and matches the stated intent.
- Treat inconclusive or unverifiable evidence as a blocking concern rather than a pass.
- Return structured findings with a risk assessment. Use severity "error" or "warning" for anything that must block publication, and return an empty findings list only when the candidate is fully verified.`,
		shortSHA(seal.SHA), baseSHA, reviewedScope,
	)
	prompt += userIntentPromptSection(sctx)
	return prompt
}
