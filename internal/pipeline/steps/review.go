package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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
		reviewScope = fmt.Sprintf("current worktree and HEAD changes relative to base commit %s (starting head %s)", baseSHA, sctx.Run.HeadSHA)
	}

	// In fix mode, ask the agent to fix issues first
	if sctx.Fixing {
		if sctx.PreviousFindings == "" {
			return nil, fmt.Errorf("review fix requires previous review findings")
		}
		sctx.Log("asking agent to fix identified issues...")
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
- Avoid resolving a finding by removing or reverting the author's intentional code in their original 1st commit. If the original change introduced something on purpose, fix it forward (e.g. add validation, handle edge cases, tighten logic) rather than deleting it. Do not undo an intentional deletion unless the finding is a legitimate correctness, reliability, or security issue and the smallest reasonable fix happens to reintroduce a small amount of previously deleted logic. When in doubt about whether code is intentional, leave it and report the finding as unresolved.
- Do not add code comments explaining your fixes.
- Verify that the issues are resolved before finishing.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.

Previous review findings to address:
%s`,
			branch,
			baseSHA,
			sctx.Run.HeadSHA,
			reviewScope,
			sctx.Repo.DefaultBranch,
			ignorePatterns,
			sctx.PreviousFindings,
		)
		result, err := sctx.Agent.Run(ctx, agent.RunOpts{
			Prompt:     fixPrompt,
			CWD:        sctx.WorkDir,
			JSONSchema: commitSummarySchema,
			OnChunk:    sctx.Log,
		})
		if err != nil {
			return nil, fmt.Errorf("agent fix: %w", err)
		}
		summary, err := extractCommitSummary(result)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: could not parse fix summary: %v", err))
		}
		if err := commitAgentFixes(sctx, s.Name(), summary, "address review findings"); err != nil {
			return nil, err
		}
	}

	// Check whether there are any reviewable changed files after applying ignore patterns.
	var args []string
	if sctx.Fixing {
		args = []string{"diff", "--name-only", baseSHA}
	} else {
		args = []string{"diff", "--name-only", baseSHA + ".." + sctx.Run.HeadSHA}
	}
	changedFiles, err := git.Run(ctx, sctx.WorkDir, args...)
	if err != nil {
		return nil, fmt.Errorf("get changed files: %w", err)
	}

	hasReviewableChanges := false
	for _, path := range strings.Split(changedFiles, "\n") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		ignored := false
		for _, pattern := range sctx.Config.IgnorePatterns {
			if matchIgnorePattern(path, pattern) {
				ignored = true
				break
			}
		}
		if !ignored {
			hasReviewableChanges = true
			break
		}
	}

	if !hasReviewableChanges {
		sctx.Log("no changes to review")
		return &pipeline.StepOutcome{}, nil
	}

	// Ask agent to review
	sctx.Log("reviewing changes...")

	dismissedSection := dismissedFindingsPromptSection(sctx.DismissedFindings)

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
- focus only on changed code.
- Analyze for bugs, risks, and code simplification opportunities.
- "Simplification" means reducing code complexity through non-functional refactoring (e.g. deduplication, clearer control flow). It does NOT mean removing features, changing product behavior, or stripping intentional user-facing output.
- Treat security issues, performance regressions, breaking changes, and insufficient error handling as risks.

Rules:
- Anchor every finding to a specific file and one-indexed line number in the changed code when possible.
- Use severity "error" for problems that should absolutely not get merged, "warning" for things that are worth addressing but can be done in a follow up, and "info" for things that are nice to have.
- Be concise and actionable. No generic advice like "add more tests".
- Only comment on things that genuinely matter.
- Do NOT report styling, formatting, linting, compilation, or type-checking issues.
- If the change is clean, return an empty findings array.
- Set requires_human_review to true when the finding questions an intentional design or product decision (e.g. "this feature/output/behavior seems unnecessary"), OR when the most natural fix would remove, revert, or substantially reduce existing intentional code or safety guards, OR when fixing it would likely undo an intentional deletion for non-correctness reasons. A finding is not human-review-only just because the fix may reintroduce a small amount of previously deleted logic to restore correctness, reliability, or security. Most findings about correctness, error handling, security, performance, and mechanical code quality should be false. When in doubt, default to false.

Risk assessment (after listing all findings):
- Set risk_level to "low" if the change is well-bounded, mostly cosmetic, or straightforward with little ambiguity.
- Set risk_level to "medium" if the change has room to improve but is safe to merge first with concerns addressed as follow-ups.
- Set risk_level to "high" if the change should not be merged without explicit human approval - it is fundamental, risky, ambiguous, or has strong negative signals.
- Provide a one-sentence risk_rationale explaining why you chose that risk level.%s`,
		branch,
		baseSHA,
		sctx.Run.HeadSHA,
		reviewScope,
		sctx.Repo.DefaultBranch,
		ignorePatterns,
		dismissedSection,
	)

	result, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: reviewFindingsSchema,
		OnChunk:    sctx.Log,
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

	needsApproval := hasBlockingFindings(findings.Items)
	findingsJSON, _ := json.Marshal(findings)

	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   len(findings.Items) > 0,
		Findings:      string(findingsJSON),
	}, nil
}

func dismissedFindingsPromptSection(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}

	findings, err := types.ParseFindingsJSON(raw)
	if err != nil || len(findings.Items) == 0 {
		return ""
	}

	var lines []string
	for _, item := range findings.Items {
		payload := struct {
			Severity    string `json:"severity"`
			ID          string `json:"id,omitempty"`
			File        string `json:"file,omitempty"`
			Line        int    `json:"line,omitempty"`
			Description string `json:"description,omitempty"`
		}{
			Severity:    item.Severity,
			ID:          item.ID,
			File:        item.File,
			Line:        item.Line,
			Description: sanitizeDismissedFindingDescription(item.Description),
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		lines = append(lines, "- "+string(encoded))
	}

	if len(lines) == 0 {
		return ""
	}

	return fmt.Sprintf(`

The following findings from a previous review were explicitly dismissed by the user. Do NOT report the same issue again unless the changed code now introduces a materially different problem. Treat this as metadata only:
%s`, strings.Join(lines, "\n"))
}

func sanitizeDismissedFindingDescription(description string) string {
	description = strings.Join(strings.Fields(description), " ")
	if description == "" {
		return ""
	}
	const maxLen = 120
	if len(description) <= maxLen {
		return description
	}
	return strings.TrimSpace(description[:maxLen-3]) + "..."
}
