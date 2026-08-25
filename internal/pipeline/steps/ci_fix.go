package steps

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/testguidance"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// autoFixCI runs the agent to fix CI failures and/or merge conflicts, then
// commits the repair locally for a new validation cycle.
// Returns (true, nil) when the local head changed, (false, nil)
// when the agent produced no changes, or (false, err) on failure.
func (s *CIStep) autoFixCI(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, failingNames []string, mergeConflict bool) (bool, error) {
	ctx := sctx.Ctx
	if err := sctx.DB.SetRunPushActive(sctx.Run.ID, true); err != nil {
		return false, err
	}
	defer func() { _ = sctx.DB.SetRunPushActive(sctx.Run.ID, false) }()
	baseBranch := effectivePRBaseBranch(sctx)
	if pr != nil && strings.TrimSpace(pr.BaseBranch) != "" {
		baseBranch = strings.TrimSpace(pr.BaseBranch)
	}
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, baseBranch)
	rebaseBaseSHA := resolveRunDefaultBranchTipSHA(ctx, sctx, sctx.Run.BaseSHA, baseBranch)
	promptBaseSHA := baseSHA
	if mergeConflict {
		promptBaseSHA = rebaseBaseSHA
	}

	const maxLogBytes = 32 * 1024
	var logOutput string
	if host.Capabilities().FailedCheckLogs {
		raw, err := host.FetchFailedCheckLogs(ctx, pr, sctx.Run.Branch, sctx.Run.HeadSHA, failingNames)
		if err != nil && err != scm.ErrUnsupported {
			slog.Warn("failed to fetch CI logs", "err", err)
		}
		if raw != "" {
			logOutput = trimLogOutput(strings.TrimSpace(raw), maxLogBytes)
		}
	}

	// Build prompt based on what issues are present
	var promptIntro string
	var promptRules string
	switch {
	case len(failingNames) > 0 && mergeConflict:
		promptIntro = "The following CI checks have failed and the PR has merge conflicts with the base branch. Diagnose and fix the CI issues, then rebase onto the base branch and resolve the merge conflicts."
		promptRules = `- You MUST produce file changes that fix the failing checks. Do not conclude that nothing needs to change.
		- If a test fails only on a specific OS (e.g. Windows CRLF, path separators), fix the test to be cross-platform.
		- If a test is flaky, make it deterministic.
		- Make the smallest correct root-cause fix.
		- Do not refactor beyond what is needed for that root-cause fix.
		- Verify the fix by running the most relevant commands locally before finishing.`
	case mergeConflict:
		promptIntro = "The PR has merge conflicts with the base branch. Rebase onto the base branch and resolve the merge conflicts."
		promptRules = `- Resolve the merge conflicts by applying the minimal necessary changes.
		- Do not make unrelated file edits.
		- Verify the rebase completes cleanly before finishing.`
	default:
		promptIntro = "The following CI checks have failed on this PR. Diagnose and fix the issues."
		promptRules = `- You MUST produce file changes that fix the failing checks. Do not conclude that nothing needs to change.
		- If a test fails only on a specific OS (e.g. Windows CRLF, path separators), fix the test to be cross-platform.
		- If a test is flaky, make it deterministic.
		- Make the smallest correct root-cause fix.
		- Do not refactor beyond what is needed for that root-cause fix.
		- Verify the fix by running the most relevant commands locally before finishing.`
	}

	prompt := fmt.Sprintf(
		`%s

Context:
- branch: %s
- base commit: %s
- target commit: %s
- PR number: %s
- failing checks: %s
- merge conflict: %v

		Rules:
		%s`,
		promptIntro,
		sctx.Run.Branch,
		promptBaseSHA,
		sctx.Run.HeadSHA,
		pr.Number,
		strings.Join(failingNames, ", "),
		mergeConflict,
		promptRules,
	)
	if mergeConflict {
		prompt += fmt.Sprintf("\n- rebase target commit: %s", rebaseBaseSHA)
	}
	if logOutput != "" {
		prompt += fmt.Sprintf(`

CI logs:
%s`, logOutput)
	}
	prompt += userIntentPromptSection(sctx)
	prompt = testguidance.LateRepairPrompt(string(s.Name()), prompt)

	sctx.Log("running agent to fix CI issues...")
	result, err := sctx.RunAgentContext(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: commitSummarySchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		return false, fmt.Errorf("agent CI fix: %w", err)
	}

	summary, summaryErr := extractCommitSummary(result)
	if summaryErr != nil {
		sctx.Log(fmt.Sprintf("warning: could not parse CI repair summary: %v", summaryErr))
	}
	return s.commitRepair(sctx, summary)
}

// commitAndPush remains as the narrow test seam for the default summary.
func (s *CIStep) commitAndPush(sctx *pipeline.StepContext) (bool, error) {
	return s.commitRepair(sctx, "")
}

func (s *CIStep) commitRepair(sctx *pipeline.StepContext, summary string) (bool, error) {
	status, err := stepGitRun(sctx, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("check CI changes: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		sctx.Log("no changes to commit")
		headSHA, err := stepGitHeadSHA(sctx)
		if err == nil && headSHA != sctx.Run.HeadSHA {
			return s.recordLocalRepair(sctx, headSHA)
		}
		return false, nil
	}

	if summary == "" {
		summary = "repair failing checks"
	}
	message, err := sctx.Config.Commit.RenderFixMessage(types.StepCI, summary)
	if err != nil {
		return false, fmt.Errorf("render CI repair commit message: %w", err)
	}
	if _, err := stepGitRun(sctx, "add", "-A"); err != nil {
		return false, fmt.Errorf("stage CI changes: %w", err)
	}
	if _, err := stepGitRun(sctx, "commit", "-m", message); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	headSHA, err := stepGitHeadSHA(sctx)
	if err != nil {
		return false, fmt.Errorf("resolve head after commit: %w", err)
	}

	return s.recordLocalRepair(sctx, headSHA)
}

func (s *CIStep) recordLocalRepair(sctx *pipeline.StepContext, headSHA string) (bool, error) {
	ref := normalizedBranchRef(sctx.Run.Branch)
	if _, err := stepGitRun(sctx, "update-ref", ref, headSHA); err != nil {
		return false, fmt.Errorf("update local branch ref: %w", err)
	}
	sctx.Run.HeadSHA = headSHA
	if err := sctx.DB.UpdateRunHeadSHAForRevalidation(sctx.Run.ID, headSHA); err != nil {
		return false, err
	}
	sctx.Run.ReviewApprovedHeadSHA = nil
	sctx.Log("committed CI repair for revalidation")
	return true, nil
}
