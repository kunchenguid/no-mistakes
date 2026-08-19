package steps

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/testguidance"
)

// autoFixCI runs the agent to fix CI failures and/or merge conflicts, then
// commits and pushes to the configured push remote.
// Returns (true, nil) when changes were committed and pushed, (false, nil)
// when the agent produced no changes, or (false, err) on failure.
func (s *CIStep) autoFixCI(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, failingNames []string, mergeConflict bool) (bool, error) {
	if ciFleetRun(sctx) {
		return false, fmt.Errorf("CI auto-fix is unavailable for a certified review-fleet run")
	}
	ctx := sctx.Ctx
	if err := sctx.DB.SetRunPushActive(sctx.Run.ID, true); err != nil {
		return false, err
	}
	defer func() { _ = sctx.DB.SetRunPushActive(sctx.Run.ID, false) }()
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	rebaseBaseSHA := resolveRunDefaultBranchTipSHA(ctx, sctx, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
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

// commitAndPush commits any uncommitted changes and force-pushes to the
// configured push remote.
// Returns (true, nil) when changes were pushed, (false, nil) when there was
// nothing to commit, or (false, err) on failure.
func (s *CIStep) commitAndPush(sctx *pipeline.StepContext) (bool, error) {
	if ciFleetRun(sctx) {
		return false, fmt.Errorf("CI auto-push is unavailable for a certified review-fleet run")
	}
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

func ciFleetRun(sctx *pipeline.StepContext) bool {
	if sctx == nil || sctx.Run == nil {
		return false
	}
	if sctx.Run.ReviewFleetEnabled || sctx.Run.CertifiedHeadSHA != nil {
		return true
	}
	if sctx.DB == nil {
		return false
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	return err == nil && run != nil && (run.ReviewFleetEnabled || run.CertifiedHeadSHA != nil)
}

func assertFleetTerminalCertification(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) error {
	if !ciFleetRun(sctx) {
		return nil
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		return fmt.Errorf("load durable certification before terminal PR state: %w", err)
	}
	if run == nil || run.CertifiedHeadSHA == nil || strings.TrimSpace(*run.CertifiedHeadSHA) == "" {
		return fmt.Errorf("refusing terminal PR state: run has no durably recorded certified head")
	}
	if err := assertCleanExactHead(sctx, strings.TrimSpace(*run.CertifiedHeadSHA), "terminal PR state"); err != nil {
		return err
	}
	if run.LastPushedSHA == nil || strings.TrimSpace(*run.LastPushedSHA) != strings.TrimSpace(*run.CertifiedHeadSHA) {
		return fmt.Errorf("refusing terminal PR state: certified head was not exactly published")
	}
	headReader, ok := host.(scm.PRHeadReader)
	if !ok {
		return fmt.Errorf("refusing terminal PR state: provider cannot prove the PR source commit")
	}
	headSHA, err := headReader.GetPRHeadSHA(sctx.Ctx, pr)
	if err != nil {
		return fmt.Errorf("read terminal PR source commit: %w", err)
	}
	if strings.TrimSpace(headSHA) != strings.TrimSpace(*run.CertifiedHeadSHA) {
		return fmt.Errorf("refusing terminal PR state: provider source commit %s does not equal certified head %s", shortObjectID(headSHA), shortObjectID(*run.CertifiedHeadSHA))
	}
	return nil
}

func (s *CIStep) pushUpdatedHeadSHA(sctx *pipeline.StepContext, newHeadSHA string) (bool, error) {
	ref := normalizedBranchRef(sctx.Run.Branch)
	pushURL := resolvePushURL(sctx)

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
	targetKind := "upstream"
	if strings.TrimSpace(sctx.Repo.ForkURL) != "" {
		targetKind = "fork"
	}
	persistBinding := func() error {
		remoteOut, err := stepGitRun(sctx, "ls-remote", pushURL, ref)
		if err != nil {
			return fmt.Errorf("verify successful push: %w", err)
		}
		fields := strings.Fields(remoteOut)
		if len(fields) == 0 || fields[0] != newHeadSHA {
			observed := "missing"
			if len(fields) > 0 {
				observed = fields[0]
			}
			return fmt.Errorf("verify successful push: remote head %s does not equal pushed head %s", observed, newHeadSHA)
		}
		return sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{
			HeadSHA:           newHeadSHA,
			TargetKind:        targetKind,
			TargetFingerprint: branchsync.TargetFingerprint(pushURL),
			Ref:               ref,
		})
	}
	if decision.upToDate {
		if err := persistBinding(); err != nil {
			return false, err
		}
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
	if err := persistBinding(); err != nil {
		return false, err
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
