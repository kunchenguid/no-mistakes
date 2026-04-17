package steps

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// autoFixCI runs the agent to fix CI failures and/or merge conflicts, then commits and pushes.
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
		- Make the minimal change needed.
		- Do not refactor beyond what is needed.
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
		- Make the minimal change needed.
		- Do not refactor beyond what is needed.
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

	sctx.Log("running agent to fix CI issues...")
	_, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:  prompt,
		CWD:     sctx.WorkDir,
		OnChunk: sctx.LogChunk,
	})
	if err != nil {
		return false, fmt.Errorf("agent CI fix: %w", err)
	}

	return s.commitAndPush(sctx)
}

// commitAndPush commits any uncommitted changes and force-pushes to upstream.
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

	upstreamSHA, lsErr := stepGitLsRemote(sctx, sctx.Repo.UpstreamURL, ref)
	if lsErr != nil {
		slog.Warn("ls-remote failed, pushing without force-with-lease", "ref", ref, "error", lsErr)
	} else if upstreamSHA == newHeadSHA {
		if _, err := stepGitRun(sctx, "update-ref", ref, newHeadSHA); err != nil {
			return false, fmt.Errorf("update local branch ref: %w", err)
		}
		sctx.Run.HeadSHA = newHeadSHA
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, newHeadSHA); err != nil {
			return false, err
		}
		return false, nil
	}
	if err := stepGitPush(sctx, sctx.Repo.UpstreamURL, ref, upstreamSHA, upstreamSHA != ""); err != nil {
		if lsErr != nil {
			return false, fmt.Errorf("push (ls-remote failed: %v): %w", lsErr, err)
		}
		return false, fmt.Errorf("push: %w", err)
	}

	if _, err := stepGitRun(sctx, "update-ref", ref, newHeadSHA); err != nil {
		return false, fmt.Errorf("update local branch ref: %w", err)
	}
	sctx.Run.HeadSHA = newHeadSHA
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, newHeadSHA); err != nil {
		return false, err
	}

	sctx.Log("committed and pushed fixes")
	return true, nil
}
