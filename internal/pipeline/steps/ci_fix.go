package steps

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// autoFixCI runs the agent to fix CI failures and/or merge conflicts, then
// commits and pushes to the configured push remote.
// Returns (true, nil) when changes were committed and pushed, (false, nil)
// when the agent produced no changes, or (false, err) on failure.
func (s *CIStep) autoFixCI(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, failingNames []string, mergeConflict bool) (bool, error) {
	ctx := sctx.Ctx
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	rebaseBaseSHA := resolveDefaultBranchTipSHA(ctx, sctx.WorkDir, sctx.Repo.UpstreamURL, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
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

	tier := s.ciRepairTier(sctx)
	sctx.Log(fmt.Sprintf("running agent to fix CI issues (tier %d)...", tier))
	_, err := sctx.InvokeAgentTier(types.PurposeUnstructuredCIRepair, tier, agent.RunOpts{
		Prompt:  prompt,
		CWD:     sctx.WorkDir,
		OnChunk: sctx.LogChunk,
	})
	if err != nil {
		return false, fmt.Errorf("agent CI fix: %w", err)
	}

	// When the agent produced new changes, run local deterministic checks and a
	// fresh strong verifier BEFORE any commit or remote update. Unverified or
	// failing work is reverted and fails closed, so CI never republishes an
	// unreviewed patch.
	status, err := stepGitRun(sctx, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("check CI changes: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		if verr := s.verifyCIPatch(sctx, baseSHA); verr != nil {
			_, _ = stepGitRun(sctx, "reset", "--hard")
			_, _ = stepGitRun(sctx, "clean", "-fd")
			return false, fmt.Errorf("CI patch failed verification: %w", verr)
		}
	}

	return s.commitAndPush(sctx)
}

// ciRepairTier escalates the CI repair from fix_balanced to authority_strong as
// auto-fix attempts accumulate, mirroring pre-publish repair. The first attempt
// starts at fix_balanced; later attempts climb the routed cascade.
func (s *CIStep) ciRepairTier(sctx *pipeline.StepContext) int {
	maxTier := 0
	if sctx.Config != nil {
		if profiles, err := sctx.Config.Routing.ResolveRoute(types.PurposeUnstructuredCIRepair); err == nil && len(profiles) > 0 {
			maxTier = len(profiles) - 1
		}
	}
	tier := s.ciFixAttempts - 1
	if tier < 0 {
		tier = 0
	}
	if tier > maxTier {
		tier = maxTier
	}
	return tier
}

// verifyCIPatch runs the configured local deterministic checks and a fresh
// strong verifier over the uncommitted CI patch. Any failing check, an
// inconclusive verifier, or a blocking finding is returned as an error so the
// caller fails closed without publishing.
func (s *CIStep) verifyCIPatch(sctx *pipeline.StepContext, baseSHA string) error {
	for _, chk := range []struct{ name, cmd string }{
		{"test", sctx.Config.Commands.Test},
		{"lint", sctx.Config.Commands.Lint},
	} {
		if strings.TrimSpace(chk.cmd) == "" {
			continue
		}
		sctx.Log(fmt.Sprintf("running local %s check on CI patch...", chk.name))
		output, exitCode, err := runStepShellCommand(sctx, chk.cmd)
		if err != nil {
			return fmt.Errorf("run %s check: %w", chk.name, err)
		}
		if exitCode != 0 {
			return fmt.Errorf("local %s check failed (exit %d)", chk.name, exitCode)
		}
		_ = output
	}

	result, err := sctx.InvokeAgent(types.PurposeEscalatedAggregateVerification, agent.RunOpts{
		Prompt:     buildCIVerifyPrompt(sctx, baseSHA),
		CWD:        sctx.WorkDir,
		JSONSchema: findingsSchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		return fmt.Errorf("strong verifier inconclusive: %w", err)
	}
	if result == nil || result.Output == nil {
		return fmt.Errorf("strong verifier returned no structured findings")
	}
	findings, err := types.ParseFindingsJSON(string(result.Output))
	if err != nil {
		return fmt.Errorf("strong verifier returned unparseable output: %w", err)
	}
	if hasBlockingFindings(findings.Items) {
		return fmt.Errorf("strong verifier rejected the CI patch: %s", findings.Summary)
	}
	return nil
}

func buildCIVerifyPrompt(sctx *pipeline.StepContext, baseSHA string) string {
	prompt := fmt.Sprintf(
		`You are independently verifying a CI-repair patch before it is republished.

Base commit: %s

The uncommitted changes in the worktree were produced to fix failing CI checks or a merge conflict. Verify them:
- Confirm the patch actually addresses the failure without introducing correctness, security, or data-loss regressions.
- Confirm the change is internally coherent and preserves the intent of the original work.
- Treat inconclusive or unverifiable evidence as a blocking concern rather than a pass.

Return structured findings. Use severity "error" or "warning" for anything that must block republishing, and return an empty findings list only when the patch is fully verified.`,
		baseSHA,
	)
	prompt += userIntentPromptSection(sctx)
	return prompt
}

// commitAndPush commits any uncommitted changes and force-pushes to the
// configured push remote.
// Returns (true, nil) when changes were pushed, (false, nil) when there was
// nothing to commit, or (false, err) on failure.
func (s *CIStep) commitAndPush(sctx *pipeline.StepContext) (bool, error) {
	status, err := stepGitRun(sctx, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("check CI changes: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		sctx.Log("no changes to commit")
		headSHA, err := stepGitHeadSHA(sctx)
		if err == nil && headSHA != sctx.Run.HeadSHA {
			return s.pushUpdatedHeadSHA(sctx, headSHA)
		}
		return false, nil
	}

	if _, err := stepGitRun(sctx, "add", "-A"); err != nil {
		return false, fmt.Errorf("stage CI changes: %w", err)
	}
	if _, err := stepGitRun(sctx, "commit", "-m", "no-mistakes: apply CI fixes"); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	headSHA, err := stepGitHeadSHA(sctx)
	if err != nil {
		return false, fmt.Errorf("resolve head after commit: %w", err)
	}

	return s.pushUpdatedHeadSHA(sctx, headSHA)
}

func (s *CIStep) pushUpdatedHeadSHA(sctx *pipeline.StepContext, newHeadSHA string) (bool, error) {
	ref := normalizedBranchRef(sctx.Run.Branch)
	pushURL := sctx.Repo.PushURL()

	// Anchor the force-with-lease to the head the run last recorded for this
	// branch (what the pipeline last pushed/observed), NOT to a SHA freshly read
	// from the remote a moment before pushing - that self-defeating anchor always
	// passes and lets an auto-fix rebased from stale local state overwrite a
	// commit that reached origin out of band. resolveForcePushDecision refuses
	// the push when the remote carries commits this run never incorporated.
	gitRun := func(args ...string) (string, error) { return stepGitRun(sctx, args...) }
	decision, err := resolveForcePushDecision(gitRun, pushURL, ref, newHeadSHA, sctx.Run.HeadSHA, sctx.Run.BaseSHA)
	if err != nil {
		return false, err
	}
	if decision.upToDate {
		if _, err := stepGitRun(sctx, "update-ref", ref, newHeadSHA); err != nil {
			return false, fmt.Errorf("update local branch ref: %w", err)
		}
		sctx.Run.HeadSHA = newHeadSHA
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, newHeadSHA); err != nil {
			return false, err
		}
		return false, nil
	}
	if err := stepGitPush(sctx, pushURL, ref, decision.remoteSHA, !decision.newBranch); err != nil {
		return false, fmt.Errorf("push: %w", err)
	}

	if _, err := stepGitRun(sctx, "update-ref", ref, newHeadSHA); err != nil {
		return false, fmt.Errorf("update local branch ref: %w", err)
	}
	sctx.Run.HeadSHA = newHeadSHA
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, newHeadSHA); err != nil {
		return false, err
	}
	// Seal the exact republished candidate. Best-effort: the verified SHA is
	// already published under the force-with-lease protections, so a
	// bookkeeping-seal failure must not fail an otherwise successful republish.
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, newHeadSHA, "ci_republish"); err != nil {
		slog.Warn("failed to seal CI republished candidate", "run", sctx.Run.ID, "sha", newHeadSHA, "error", err)
	}

	sctx.Log("committed and pushed fixes")
	return true, nil
}
