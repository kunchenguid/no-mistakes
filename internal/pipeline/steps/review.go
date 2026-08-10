package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testguidance"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ReviewStep reviews the diff for bugs, security issues, and doc gaps.
type ReviewStep struct{}

func (s *ReviewStep) Name() types.StepName { return types.StepReview }

func (s *ReviewStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	branch := sctx.Run.Branch
	ignorePatterns := "none"
	if len(sctx.Config.IgnorePatterns) > 0 {
		ignorePatterns = strings.Join(sctx.Config.IgnorePatterns, ", ")
	}

	reviewScope := fmt.Sprintf("branch changes between %s and %s", baseSHA, sctx.Run.HeadSHA)
	if sctx.Fixing {
		startingHeadSHA := sctx.ReviewStartingHeadSHA
		if startingHeadSHA == "" {
			startingHeadSHA = sctx.Run.HeadSHA
		}
		reviewScope = fmt.Sprintf("current worktree and HEAD changes relative to base commit %s (starting head %s)", baseSHA, startingHeadSHA)
	}

	// Bounded workload size (changed files + net lines) for local telemetry, so
	// review/fix efficiency can be normalized without external git archaeology.
	// Best-effort: a diff-stat failure leaves the workload unknown.
	workload := reviewWorkload(ctx, sctx.WorkDir, baseSHA, sctx.Run.HeadSHA)

	// In fix mode, ask the agent to fix issues first.
	//
	// The verification-discipline rules below (apply all fixes first, then one
	// focused verification of the changed area, and never run the whole repo
	// test/lint suite in the fixer round) exist for wall-clock reasons: a
	// forensic audit of a real multi-round run measured the fixer re-running the
	// entire test+lint suite ~5x per round (27 runs across 5 rounds, ~784s of
	// the 2419s review step), plus the model round-trips that poll those long
	// subprocesses. Review runs before the dedicated Test and Lint steps
	// (pipeline order in common.go), which are the authoritative test and lint
	// gates; their coverage may be focused when the repository has no configured
	// commands. The fixer prohibition stays universal because the fixer only
	// needs to confirm its own edits hold, not re-gate the whole repository. This
	// mirrors the same "relevant"-scoped, cross-tool-forbidden discipline the
	// test and lint fix prompts already carry. The instruction is a contract,
	// not an enforced sandbox - the agent has free shell access - so the pinned
	// regression tests guard the wording, not the runtime.
	var fixSummary string
	if sctx.Fixing && !sctx.SkipFixExecution {
		previousFindings := sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings)
		historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx) + testguidance.Rule
		fixPrompt := fmt.Sprintf(
			`Investigate previous review findings and address legitimate ones.

Examine the relevant code yourself and apply fixes directly.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- review scope: %s
- default branch: %s
- ignore patterns: %s

Rules:
- Always start with double checking whether the findings are legitimate.
- Before changing code, identify whether each finding is a local defect or a symptom of a deeper design, abstraction, validation, ownership, or test-coverage flaw. Prefer the smallest correct root-cause fix within the changed area over patching only the reported line.
- If a narrow fix would leave the same class of bug likely elsewhere, fix the deepest practical cause instead.
- Avoid resolving a finding by removing or reverting the author's intentional code in their original 1st commit. If the original change introduced something on purpose, fix it forward (e.g. add validation, handle edge cases, tighten logic) rather than deleting it. Similarly, if the original change intentionally deleted or simplified code, do not restore or re-add the removed code unless the finding is a legitimate correctness, reliability, or security issue and the smallest reasonable fix happens to reintroduce a small amount of previously deleted logic. When in doubt about whether code is intentional, leave it and report the finding as unresolved.
- Do not add code comments explaining your fixes.
- Apply all the fixes you intend to make first; do not run any verification in between individual fixes.
- After all fixes are applied, run one focused verification limited to the changed area (the specific package, file, or test you touched) at the end of the fix round to confirm the fixes hold.
- Do NOT run the complete repository test suite or lint suite during this fix round. The pipeline has dedicated test and lint steps after review that are the authoritative test and lint gates; their coverage may itself be focused on the changed area when the repository has no configured test or lint commands.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s

Previous review findings to address:
%s`,
			branch,
			baseSHA,
			sctx.Run.HeadSHA,
			reviewScope,
			sctx.Repo.DefaultBranch,
			ignorePatterns,
			historySection,
			previousFindings,
		)
		summary, err := executeFixMode(sctx, s.Name(), fixExecutionOptions{
			RequirePreviousFindings: true,
			MissingFindingsError:    "review fix requires previous review findings",
			LogMessage:              "asking agent to fix identified issues...",
			Prompt:                  fixPrompt,
			ErrorPrefix:             "agent fix",
			FallbackSummary:         "address review findings",
			SessionRole:             pipeline.SessionRoleFixer,
			Purpose:                 "review-fix",
			Workload:                workload,
		})
		if err != nil {
			return nil, err
		}
		fixSummary = summary
	}
	reviewTargetSHA := sctx.Run.HeadSHA

	// The changed-file set is read once and viewed two ways on purpose: the
	// ignore-filtered subset decides whether there is anything to review, while
	// trusted path instructions are selected against the complete set (see
	// matchPathInstructions).
	var args []string
	if sctx.Fixing {
		args = []string{"diff", "--name-only", "-z", "--no-renames", baseSHA}
	} else {
		args = []string{"diff", "--name-only", "-z", "--no-renames", baseSHA + ".." + sctx.Run.HeadSHA}
	}
	changedFiles, err := git.Run(ctx, sctx.WorkDir, args...)
	if err != nil {
		return nil, fmt.Errorf("get changed files: %w", err)
	}
	changed := changedPathList(changedFiles)

	if len(reviewablePaths(changed, sctx.Config.IgnorePatterns)) == 0 {
		sctx.Log("no changes to review")
		noChangeFindings := Findings{
			RiskLevel:     "low",
			RiskRationale: "no reviewable changes",
		}
		findingsJSON, _ := json.Marshal(noChangeFindings)
		return approvedReviewOutcome(reviewTargetSHA, &pipeline.StepOutcome{
			Findings:   string(findingsJSON),
			FixSummary: fixSummary,
		})
	}

	// Ask agent to review
	sctx.Log("reviewing changes...")

	// The review turn (initial and every post-fix rereview) carries the intent
	// conformance obligation: when the intent is authoritative acceptance
	// criteria (explicit --intent), a change that contradicts it must park via
	// an ask-user finding. The clause is empty for inferred intent, leaving the
	// prompt unchanged. This is what makes a fixer round that removed a
	// required behavior park instead of silently completing.
	//
	// Review is always pre-push (StepReview.Order < StepPush/PR/CI). The phase
	// clause and the post-parse strip below keep pipeline-owned delivery
	// outcomes (remote branch, PR, CI for this run) out of source-review
	// findings; later steps own those. External / pre-existing lifecycle
	// requirements stay in scope.
	//
	// TODO(intent-conformance-C, HELD): add the deterministic, zero-LLM
	// net-deleted-author-lines git-diff backstop for the removal-of-required
	// class - a fixer round that net-deletes author-added lines parks
	// regardless of intent source. Held pending a scope decision.
	historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + fixRoundProvenanceClause(sctx) + userIntentPromptSection(sctx) + intentConformanceReviewClause(sctx) + pipelineDeliveryPhaseClause() + testguidance.Rule + testguidance.ReviewerAction

	// Path-scoped repository review guidance, taken from the trusted
	// default-branch config copy (regardless of allow_repo_commands) so a pushed
	// branch cannot steer the reviewer that gates it. Selection runs against the
	// complete changed-file set, never the ignore-filtered one, so a pushed
	// ignore_patterns entry cannot suppress a trusted rule. Only blocks whose
	// glob matches a changed path are appended, so a repository with none
	// configured - or none relevant to this diff - gets the prompt above
	// unchanged.
	pathInstructionMatches := matchPathInstructions(changed, sctx.Config.Review.PathInstructions)
	logPathInstructions(sctx.Log, pathInstructionMatches)
	pathInstructions := reviewPathInstructionsSection(pathInstructionMatches)

	prompt := fmt.Sprintf(
		`Review the code changes and return structured findings with a risk assessment.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- review scope: %s
- default branch: %s
- ignore patterns: %s

Task:
- Read the relevant history and diff yourself.
- Focus findings on risks introduced by changed code, but inspect surrounding code, call sites, shared helpers, tests, and invariants when needed to understand root cause.
- Determine from the stated intent and relevant evidence whether a bug-fix change claims a durable fix or explicitly authorized short-term containment.
- For a claimed durable fix, reconstruct the concrete failing sequence and required invariant, inspect relevant sibling paths and shared state transitions, and ask whether the same authorized failure remains reachable.
- When source evidence proves the failure remains reachable, report the concrete path and recommend the earliest supported shared boundary that would make the invariant hold, rather than duplicating another symptom patch.
- Do not infer a systemic flaw from code shape, duplication, or architectural preference alone. Do not demand a shared abstraction or broad redesign without a concrete reachable path, violated invariant, or immediately competing semantic owner.
- Do not block explicitly authorized honest containment merely because a later durable fix is possible. Do not expand user scope or turn optional broader improvements into blockers.
- Do NOT run tests during review. The pipeline has a dedicated test step after review.
- Analyze for bugs, risks, and code simplification opportunities.
- "Simplification" means reducing code complexity through non-functional refactoring (e.g. deduplication, clearer control flow). It does NOT mean removing features, changing product behavior, or stripping intentional user-facing output.
- Treat security issues, performance regressions, breaking changes, and insufficient error handling as risks.
- Do a full review pass before returning. Do not stop after the first valid finding. Continue inspecting the rest of the changed code until you have enumerated all material issues you can substantiate.

Rules:
- Anchor every finding to a specific file and one-indexed line number in the changed code when possible.
- Use severity "error" for problems that should absolutely not get merged, "warning" for things that are worth addressing but can be done in a follow up, and "info" for things that are nice to have.
- Be concise and actionable. No generic advice like "add more tests".
- Only comment on things that genuinely matter.
- Do NOT report styling, formatting, linting, compilation, or type-checking issues.
- If the change is clean, return an empty findings array.
- For each finding, set the action field to one of:
  - "ask-user": the finding is about functional requirements or product behavior, or otherwise challenges the author's deliberate intent. Even if it seems obviously wrong, we should ask the user for review. Examples: "this feature seems unnecessary", "this hardcoded value should be configurable", "this deletion looks wrong". When in doubt, default to "ask-user".
  - "auto-fix": the finding is a non-functional, non user-visible issue (correctness, error handling, security, performance, mechanical code quality) that can be safely fixed without any discussion about the author's intent.
  - "no-op": the finding is informational and does not require any action (e.g. noting a pattern, acknowledging a tradeoff).
- For each finding, set review_scope to exactly one of:
  - "source": every source-verifiable finding, including any finding that mixes a source defect with a delivery claim.
  - "pipeline-owned-delivery": only a finding whose sole claim is that this run's remote branch, push, PR, or CI output is not present yet.
  - "external-delivery": a pre-existing or external PR, third-party artifact, or other lifecycle requirement not owned by this run.

Risk assessment (after listing all findings):
- Assess source code, source-verifiable criteria, and enforceable external lifecycle requirements normally, while excluding findings scoped "pipeline-owned-delivery" from risk.
- Set risk_level to "low" if the change is well-bounded, mostly cosmetic, or straightforward with little ambiguity.
- Set risk_level to "medium" if the change has room to improve but is safe to merge first with concerns addressed as follow-ups.
- Set risk_level to "high" if the change should not be merged without explicit human approval - it is fundamental, risky, ambiguous, or has strong negative signals.
- Provide a one-sentence risk_rationale explaining why you chose that risk level.
- Set risk_scope to "source-or-external" when the assessment reflects source risk or enforceable external state, and to "pipeline-owned-delivery" only when it is based solely on a deferred outcome this run owns.%s%s`,
		branch,
		baseSHA,
		sctx.Run.HeadSHA,
		reviewScope,
		sctx.Repo.DefaultBranch,
		ignorePatterns,
		historySection,
		pathInstructions,
	)

	// Every review turn - the initial review and every post-fix rereview -
	// deliberately runs session-free. Round N's fixes implement round N-1's
	// review findings, so resuming any prior review turn's session would seat
	// the prescriber of those fixes as their certifier: the rereview then
	// verifies that its own prescription was implemented instead of judging
	// whether the pipeline-authored code is correct (the mechanism behind a
	// real shipped defect where one fix round wrote both wrong code and the
	// test blessing it, and the resumed reviewer session passed them). The
	// cross-round context a rereview legitimately needs travels in the
	// explicit sanitized round-history section above; only the fixer keeps a
	// durable session (executeFixMode), because it certifies nothing.
	result, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		Env:        sctx.Env,
		JSONSchema: reviewFindingsSchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    "review",
		Workload:   workload,
	})
	if err != nil {
		return nil, fmt.Errorf("agent review: %w", err)
	}

	// Parse structured findings
	var findings Findings
	if result.Output != nil {
		if err := json.Unmarshal(result.Output, &findings); err != nil {
			sctx.Log("could not parse structured output, using text response")
			findings = Findings{Summary: result.Text}
		}
	}

	// Phase ownership boundary: drop findings that only claim later pipeline-
	// owned delivery (push/PR/CI for this run) has not happened yet. Prompt
	// guidance alone is not enough - models still emit these under
	// authoritative intent criteria like "Open PR A unmerged".
	if stripped, n := stripDeferredPipelineOwnedDeliveryFindings(findings); n > 0 {
		sctx.Log(fmt.Sprintf("dropped %d deferred pipeline-owned delivery finding(s) (owned by later push/PR/CI steps)", n))
		findings = stripped
	}

	needsApproval := hasBlockingFindings(findings.Items)
	findingsJSON, _ := json.Marshal(findings)

	return approvedReviewOutcome(reviewTargetSHA, &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   len(findings.Items) > 0,
		Findings:      string(findingsJSON),
		FixSummary:    fixSummary,
	})
}

// fixRoundProvenanceClause reframes a rereview's fix-round changes as
// pipeline-authored code under the author-grade adversarial standard. Without
// it, the round-history section reads as "found and fixed" and invites less
// scrutiny of exactly the code the pipeline itself just wrote: the fixer
// authors both code and tests in one round, so the only independent check
// that code ever gets is this rereview. Empty outside fix mode, leaving the
// initial review prompt unchanged.
func fixRoundProvenanceClause(sctx *pipeline.StepContext) string {
	if !sctx.Fixing {
		return ""
	}
	return `

Fix-round provenance:
- This is a re-review after this run's automated fix round(s): every commit after the starting head, plus any uncommitted worktree changes, was authored by the pipeline's own fixer agent, not by the change author.
- Review that pipeline-authored code with exactly the same adversarial standard as the author's original changes. It is unreviewed new code, not a settled resolution of the findings that prompted it.
- Prior findings and fix summaries are claims, not evidence. Verify each claimed fix against the current code, and independently judge whether behavior the fix rounds introduced is correct, not merely whether it implements what was prescribed.
- A test added or changed in the same fix round as the code it exercises is part of that round's claim, not independent proof: judge whether its asserted outcome is the right outcome and whether it could still pass with the code wrong.
`
}

// approvedReviewOutcome captures the immutable commit examined by this full
// review round. The executor persists it only if this outcome ultimately
// completes the review step, so parked, failed, skipped, and superseded rounds
// cannot gain or advance approval authority.
func approvedReviewOutcome(reviewTargetSHA string, outcome *pipeline.StepOutcome) (*pipeline.StepOutcome, error) {
	outcome.ReviewApprovedHeadSHA = reviewTargetSHA
	return outcome, nil
}

func sanitizedPreviousFindingsForPrompt(raw string) string {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return sanitizePromptMultilineText(raw)
	}
	for i := range findings.Items {
		findings.Items[i].ID = sanitizePromptText(findings.Items[i].ID)
		findings.Items[i].Severity = sanitizePromptText(findings.Items[i].Severity)
		findings.Items[i].File = sanitizePromptText(findings.Items[i].File)
		findings.Items[i].Description = sanitizePromptMultilineText(findings.Items[i].Description)
		findings.Items[i].Source = sanitizePromptText(findings.Items[i].Source)
		findings.Items[i].UserInstructions = sanitizePromptMultilineText(findings.Items[i].UserInstructions)
		findings.Items[i].ReviewScope = sanitizePromptText(findings.Items[i].ReviewScope)
	}
	findings.Summary = sanitizePromptMultilineText(findings.Summary)
	findings.RiskLevel = sanitizePromptText(findings.RiskLevel)
	findings.RiskRationale = sanitizePromptMultilineText(findings.RiskRationale)
	findings.RiskScope = sanitizePromptText(findings.RiskScope)
	encoded, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return sanitizePromptMultilineText(raw)
	}
	return encoded
}

func sanitizePromptText(text string) string {
	return strings.Join(strings.Fields(sanitizePromptMultilineText(text)), " ")
}

func sanitizePromptMultilineText(text string) string {
	text = strings.NewReplacer("<<<<<<<", " ", "=======", " ", ">>>>>>>", " ").Replace(text)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.Join(strings.Fields(lines[i]), " ")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
