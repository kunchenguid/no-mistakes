package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
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

	result, err := sctx.InvokeAgent(purpose, agent.RunOpts{
		Prompt:     buildVerifyPrompt(sctx, baseSHA, seal, reviewed),
		CWD:        sctx.WorkDir,
		JSONSchema: reviewFindingsSchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		return nil, fmt.Errorf("verify: aggregate verification: %w", err)
	}
	// A missing or malformed verdict is inconclusive, which must block rather
	// than pass as verified.
	if result == nil || result.Output == nil {
		return nil, fmt.Errorf("verify: aggregate verification returned no structured findings")
	}
	var findings Findings
	if err := json.Unmarshal(result.Output, &findings); err != nil {
		return nil, fmt.Errorf("verify: verification output malformed: %w", err)
	}

	needsApproval := hasBlockingFindings(findings.Items)
	findingsJSON, _ := json.Marshal(findings)

	// A clean verification makes this the latest strong-reviewed candidate, so a
	// later identical candidate can skip re-verification.
	if !needsApproval {
		recordReviewedCandidate(sctx)
	}

	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   len(findings.Items) > 0,
		Findings:      string(findingsJSON),
	}, nil
}

// verifyPurpose selects the aggregate verification purpose. Normal verification
// uses review_strong; it escalates to authority_strong for intent-sensitive work,
// an initially high-risk review whose candidate then changed, or a re-verification
// after a blocking finding survived balanced repair (Verify running in fix mode).
func verifyPurpose(sctx *pipeline.StepContext) types.Purpose {
	if strings.TrimSpace(sctx.UserIntent) != "" || sctx.Fixing || initialReviewWasHighRisk(sctx) {
		return types.PurposeEscalatedAggregateVerification
	}
	return types.PurposeNormalAggregateVerification
}

// initialReviewWasHighRisk reports whether the recorded strong review classified
// the change as high risk. Best-effort: an unreadable record is treated as
// not-high-risk (normal verification).
func initialReviewWasHighRisk(sctx *pipeline.StepContext) bool {
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return false
	}
	for _, sr := range steps {
		if sr.StepName != types.StepReview || sr.FindingsJSON == nil {
			continue
		}
		findings, err := types.ParseFindingsJSON(*sr.FindingsJSON)
		if err != nil {
			return false
		}
		return strings.EqualFold(strings.TrimSpace(findings.RiskLevel), "high")
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
