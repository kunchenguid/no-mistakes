package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testguidance"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RebaseStep syncs the pushed branch with the configured push target and the
// latest default branch from upstream.
type RebaseStep struct{}

func (s *RebaseStep) Name() types.StepName { return types.StepRebase }

const forkBranchRefPrefix = "refs/remotes/no-mistakes-push/"

func (s *RebaseStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	defaultBranch := effectivePRBaseBranch(sctx)
	branchTarget := ""
	pushRemote := resolveUpstreamURL(sctx)
	if branch != "" {
		branchTarget = "origin/" + branch
		if strings.TrimSpace(sctx.Repo.ForkURL) != "" {
			pushRemote = sctx.Repo.PushURL()
			branchTarget = forkBranchTrackingRef(branch)
		}
	}

	// Detect force push before fetching so we can skip pushed-branch sync.
	// A force push means the user explicitly rewrote the branch - the pushed
	// commit is authoritative and must not be overwritten by prior pipeline
	// state on the remote.
	forcePush := isForcePushAgainstRemote(ctx, sctx.WorkDir, pushRemote, branch, branchTarget, sctx.Run.BaseSHA)

	sctx.Log("fetching latest upstream state...")
	if err := fetchRunUpstreamBranch(ctx, sctx, defaultBranch); err != nil {
		sctx.LogFile(fmt.Sprintf("warning: could not fetch origin/%s: %v", defaultBranch, err))
	}
	// Sync the push branch's remote-tracking ref only when we are about to rebase
	// onto it (a normal push). On a force push we deliberately skip both the fetch
	// and the rebase: the pushed commit is authoritative, and the remote-tracking
	// ref must keep pointing at the head we last *observed* rather than the live
	// tip. The push step uses that tracking ref as its force-with-lease anchor;
	// if we refreshed it here, the anchor would equal the live remote head and the
	// lease's "remote unchanged since we last saw it" fast path would pass even
	// when the remote carries an out-of-band commit - silently clobbering it
	// (the original #281/#305 hazard, in the force-push path). Leaving it stale is
	// what lets the push step's content check catch that case.
	if !forcePush && branch != "" && branch != defaultBranch {
		if strings.TrimSpace(sctx.Repo.ForkURL) == "" {
			if err := fetchRunUpstreamBranch(ctx, sctx, branch); err != nil {
				sctx.LogFile(fmt.Sprintf("warning: could not fetch origin/%s: %v", branch, err))
			}
		} else if err := git.FetchRemoteBranchToRef(ctx, sctx.WorkDir, pushRemote, branch, branchTarget); err != nil {
			sctx.LogFile(fmt.Sprintf("warning: could not fetch %s: %v", branchTarget, err))
		}
	}

	// Stop before rebasing when the gated branch carries commits that live on
	// the contributor's local default branch but were never pushed to
	// origin/<default>. Rebasing onto the fresh remote default keeps those
	// commits in the branch's history, so the PR may bundle another
	// workstream's unpushed work. Surface the ambiguity for a human decision.
	if outcome := detectBundledLocalDefaultCommits(ctx, sctx, branch, defaultBranch); outcome != nil {
		return outcome, nil
	}
	if forcePush && branch == defaultBranch && remoteDefaultBranchAdvanced(ctx, sctx.WorkDir, defaultBranch, sctx.Run.BaseSHA) {
		findingsJSON, _ := json.Marshal(Findings{
			Items: []Finding{{
				Severity:    "warning",
				File:        filepath.Join("internal", "pipeline", "steps", "rebase.go"),
				Description: fmt.Sprintf("origin/%s advanced after the force push; manual review required before updating the default branch", defaultBranch),
			}},
			Summary: fmt.Sprintf("remote %s advanced during force push", defaultBranch),
		})
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			Findings:      string(findingsJSON),
		}, nil
	}

	targets := rebaseTargetsForBranch(branch, defaultBranch, branchTarget)
	if forcePush {
		sctx.Log("force push detected, skipping " + branchTarget + " sync")
		targets = forcePushRebaseTargets(branch, defaultBranch)
	}

	merging := mergesMovedBase(sctx)

	if sctx.Fixing {
		before, err := git.HeadSHA(ctx, sctx.WorkDir)
		if err != nil {
			return nil, err
		}
		for _, target := range targets {
			var err error
			if merging {
				err = mergeWithAgent(ctx, sctx, target)
			} else {
				err = rebaseWithAgent(ctx, sctx, target)
			}
			if err != nil {
				return nil, err
			}
		}
		outcome, err := updateHeadSHA(ctx, sctx)
		if err == nil {
			if sctx.Run.HeadSHA == before {
				outcome.FixSummary = NoChangesAppliedSummary
				sctx.Log("no changes applied: branch already up to date")
			} else {
				outcome.FixSummary = changesAppliedSummary
				if merging {
					sctx.Log("merged upstream into branch")
				} else {
					sctx.Log("rebased branch onto upstream")
				}
			}
		}
		return outcome, err
	}

	// Normal mode: try all integrations, track which targets had conflicts
	var conflictTargets []string
	var conflictFindings []Finding
	for _, target := range targets {
		var conflictFiles []string
		var err error
		if merging {
			conflictFiles, err = tryMerge(ctx, sctx, target)
		} else {
			conflictFiles, err = tryRebase(ctx, sctx, target)
		}
		if err != nil {
			return nil, err
		}
		if len(conflictFiles) > 0 {
			conflictTargets = append(conflictTargets, target)
			for _, file := range conflictFiles {
				conflictFindings = append(conflictFindings, Finding{
					Severity:    "warning",
					File:        file,
					Description: fmt.Sprintf("merge conflict %s %s", integrationVerb(merging), target),
				})
			}
		}
	}

	if len(conflictTargets) > 0 {
		summary := fmt.Sprintf("conflict %s %s", integrationVerb(merging), strings.Join(conflictTargets, ", "))
		findingsJSON, _ := json.Marshal(Findings{Items: dedupeRebaseFindings(conflictFindings), Summary: summary})
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			AutoFixable:   true,
			Findings:      string(findingsJSON),
		}, nil
	}

	return updateHeadSHA(ctx, sctx)
}

// rebaseTargets returns the ordered list of refs to rebase onto.
func rebaseTargets(branch, defaultBranch string) []string {
	return rebaseTargetsForBranch(branch, defaultBranch, "origin/"+branch)
}

func rebaseTargetsForBranch(branch, defaultBranch, branchTarget string) []string {
	var targets []string
	if branch != "" && branch != defaultBranch {
		targets = append(targets, branchTarget)
	}
	if branch != defaultBranch {
		targets = append(targets, "origin/"+defaultBranch)
	}
	return targets
}

// forcePushRebaseTargets returns rebase targets for a force push. The pushed
// branch target is skipped because it may contain autofix commits from prior
// pipeline runs that the force push intended to discard.
func forcePushRebaseTargets(branch, defaultBranch string) []string {
	if branch == defaultBranch {
		return nil
	}
	return []string{"origin/" + defaultBranch}
}

// effectivePRBaseBranch resolves the branch used as the integration base for
// rebases. Per-run overrides win over repo config; the repository default
// remains the fallback when neither selects a separate PR target branch.
func effectivePRBaseBranch(sctx *pipeline.StepContext) string {
	defaultBranch := strings.TrimSpace(sctx.Repo.DefaultBranch)
	if runBase := runPRBaseBranch(sctx); runBase != "" {
		defaultBranch = runBase
	} else if sctx.Config != nil && strings.TrimSpace(sctx.Config.PR.BaseBranch) != "" {
		defaultBranch = strings.TrimSpace(sctx.Config.PR.BaseBranch)
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	return defaultBranch
}

// detectBundledLocalDefaultCommits returns a blocking finding when the gated
// branch carries commits that exist on the contributor's local default branch
// but were never pushed to origin/<default>. In multi-session / monorepo setups
// the local default branch routinely carries another workstream's unpushed
// work; branching a fix off that local tip silently drags it into the PR when
// the branch is rebased onto the remote default. Returns nil when no such
// divergence is detected so the run proceeds normally.
//
// It only flags commits the branch actually carries: it reads the local default
// tip from the working repo, confirms that tip is ahead of origin/<default> and
// is a strict ancestor of the branch HEAD, then enumerates the unpushed commits.
// Equal tips are the common commit-on-main-then-name-a-branch workflow, not
// evidence of an additional bundled workstream.
// Detection is best-effort - if the local default tip advanced past the branch
// point, or the working repo cannot be read, it returns nil rather than guess.
func detectBundledLocalDefaultCommits(ctx context.Context, sctx *pipeline.StepContext, branch, defaultBranch string) *pipeline.StepOutcome {
	if branch == "" || branch == defaultBranch {
		return nil
	}
	workingPath := strings.TrimSpace(sctx.Repo.WorkingPath)
	if workingPath == "" {
		return nil
	}
	localTip, err := git.Run(ctx, workingPath, "rev-parse", "--verify", "--quiet", "refs/heads/"+defaultBranch+"^{commit}")
	if err != nil {
		return nil
	}
	localTip = strings.TrimSpace(localTip)
	if localTip == "" {
		return nil
	}
	remoteRef := "origin/" + defaultBranch
	if _, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", "--quiet", remoteRef+"^{commit}"); err != nil {
		return nil
	}
	// The local default tip must be present in the gate's object store (it is
	// when the branch carries it as an ancestor) for the reachability checks.
	if _, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", "--quiet", localTip+"^{commit}"); err != nil {
		return nil
	}
	// Already pushed (local default not ahead of remote) -> nothing bundled.
	if isAncestor(ctx, sctx.WorkDir, localTip, remoteRef) {
		return nil
	}
	// The branch must actually carry the local default tip's commits.
	if !isAncestor(ctx, sctx.WorkDir, localTip, "HEAD") {
		return nil
	}

	// A delivery branch created at local main's tip carries only that intended
	// work, not an additional workstream beneath its own commits (#998).
	head, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err == nil && head == localTip {
		return nil
	}

	subjects, err := git.Run(ctx, sctx.WorkDir, "log", "--oneline", "--no-decorate", remoteRef+".."+localTip)
	if err != nil || strings.TrimSpace(subjects) == "" {
		return nil
	}
	commits := strings.Split(strings.TrimSpace(subjects), "\n")
	// Report the proposed PR, not a two-dot comparison that can count
	// upstream-only changes as removals from an outdated local default tip.
	base, baseErr := git.Run(ctx, sctx.WorkDir, "merge-base", remoteRef, "HEAD")
	var files []string
	var filesErr error
	if baseErr == nil {
		files, filesErr = git.DiffNameOnly(ctx, sctx.WorkDir, base, "HEAD")
	}
	fileEvidence := "PR file count unavailable"
	if baseErr == nil && filesErr == nil {
		fileEvidence = fmt.Sprintf("proposed PR changes %d file(s)", len(files))
	}
	firstFile := ""
	if len(files) > 0 {
		firstFile = files[0]
	}

	description := fmt.Sprintf(
		"branch carries %d commit(s) that exist on your local %s branch but were never pushed to origin/%s; these may be unintended bundled work (%s):\n- %s\n\nConfirm these commits belong in this PR before approving, or manually separate the intended work onto origin/%s before gating.",
		len(commits), defaultBranch, defaultBranch, fileEvidence, strings.Join(commits, "\n- "), defaultBranch,
	)
	fixSummary := ""
	if sctx.Fixing {
		fixSummary = NoChangesAppliedSummary
		const explanation = "no changes applied: bundled local-default commits require manual separation or explicit approval"
		description += "\n\n" + explanation + "; the rebase conflict resolver cannot safely select commits to discard."
		sctx.Log(explanation)
	}
	findingsJSON, _ := json.Marshal(Findings{
		Items: []Finding{{
			Severity:    "warning",
			File:        firstFile,
			Description: description,
			// Bundling another workstream's unpushed commits is a workflow call
			// the contributor must make (push <default>, rebase, or proceed); the
			// pipeline cannot safely auto-resolve it. Mark it ask-user so the gate
			// classifies it correctly and the driving agent escalates.
			Action: types.ActionAskUser,
		}},
		Summary: fmt.Sprintf("branch bundles %d unpushed %s commit(s)", len(commits), defaultBranch),
	})
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		AutoFixable:   false,
		Findings:      string(findingsJSON),
		FixSummary:    fixSummary,
	}
}

func isAncestor(ctx context.Context, workDir, ancestor, descendant string) bool {
	_, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

func remoteDefaultBranchAdvanced(ctx context.Context, workDir, defaultBranch, baseSHA string) bool {
	if baseSHA == "" || git.IsZeroSHA(baseSHA) {
		return false
	}
	remoteSHA, err := git.Run(ctx, workDir, "rev-parse", "--verify", "origin/"+defaultBranch)
	if err != nil {
		return false
	}
	return strings.TrimSpace(remoteSHA) != baseSHA
}

// isForcePush returns true when the current push is non-fast-forward relative
// to the previous push (baseSHA). This indicates the user explicitly rewrote
// history and the pipeline should treat the new HEAD as authoritative.
func isForcePush(ctx context.Context, workDir, branch, baseSHA string) bool {
	localRef := ""
	if branch != "" {
		localRef = "origin/" + branch
	}
	return isForcePushAgainstRemote(ctx, workDir, "origin", branch, localRef, baseSHA)
}

func isForcePushAgainstRemote(ctx context.Context, workDir, remote, branch, localRef, baseSHA string) bool {
	if git.IsZeroSHA(baseSHA) || baseSHA == "" {
		return false
	}
	_, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", baseSHA, "HEAD")
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return false
	}
	if branch != "" {
		remoteSHA, err := git.LsRemote(ctx, workDir, remote, "refs/heads/"+branch)
		if err == nil && remoteSHA != "" {
			_, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", remoteSHA, "HEAD")
			if err == nil {
				return false
			}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				return true
			}
		}
		if localRef != "" {
			if _, err := git.Run(ctx, workDir, "rev-parse", "--verify", localRef); err == nil {
				return isRemoteBranchRewritten(ctx, workDir, localRef)
			}
		}
	}
	return false
}

func forkBranchTrackingRef(branch string) string {
	return forkBranchRefPrefix + branch
}

func isRemoteBranchRewritten(ctx context.Context, workDir, remoteRef string) bool {
	_, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", remoteRef, "HEAD")
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}

// tryRebase attempts a rebase onto targetRef. Returns conflicted files when the
// rebase stops on merge conflicts. The rebase is aborted before returning.
func tryRebase(ctx context.Context, sctx *pipeline.StepContext, targetRef string) ([]string, error) {
	skip, err := shouldSkipRebase(ctx, sctx, targetRef)
	if err != nil {
		return nil, err
	}
	if skip {
		return nil, nil
	}

	sctx.Log(fmt.Sprintf("rebasing onto %s...", targetRef))
	if _, err := git.Run(ctx, sctx.WorkDir, "rebase", targetRef); err != nil {
		conflictFiles := rebaseConflictFiles(ctx, sctx.WorkDir)
		_, _ = git.Run(ctx, sctx.WorkDir, "rebase", "--abort")

		if len(conflictFiles) == 0 {
			return nil, fmt.Errorf("rebase onto %s: %w", targetRef, err)
		}
		return conflictFiles, nil
	}
	return nil, nil
}

// rebaseWithAgent performs a rebase and uses the agent to resolve any conflicts.
func rebaseWithAgent(ctx context.Context, sctx *pipeline.StepContext, targetRef string) error {
	skip, err := shouldSkipRebase(ctx, sctx, targetRef)
	if err != nil {
		return err
	}
	if skip {
		return nil
	}

	sctx.Log(fmt.Sprintf("rebasing onto %s...", targetRef))
	if _, err := git.Run(ctx, sctx.WorkDir, "rebase", targetRef); err == nil {
		return nil
	}

	if len(rebaseConflictFiles(ctx, sctx.WorkDir)) == 0 {
		_, _ = git.Run(ctx, sctx.WorkDir, "rebase", "--abort")
		return fmt.Errorf("rebase onto %s failed (no conflicts detected)", targetRef)
	}
	sctx.Log("conflicts detected, asking agent to resolve...")
	conflictFiles := rebaseConflictFiles(ctx, sctx.WorkDir)

	prompt := fmt.Sprintf(
		`Resolve git rebase conflicts. The rebase of the current branch onto %s has conflicts.

Current conflicted files:
- %s

Instructions:
- Find all conflicting files and resolve the conflict markers (<<<<<<< ======= >>>>>>>).
- After resolving each file, stage it with: git add <file>
- After all conflicts are resolved, run: git rebase --continue
- If additional conflicts arise during rebase --continue, resolve those too.
- Do not modify any files that don't have conflicts.
- Preserve the intent of both the current branch changes and the upstream changes.
- Return JSON with a single "summary" field describing what you resolved.
- Keep the summary under 10 words.`,
		targetRef,
		strings.Join(conflictFiles, "\n- "),
	)
	if sctx.PreviousFindings != "" {
		prompt += "\n\nPrevious findings:\n" + sctx.PreviousFindings
	}
	prompt += userIntentPromptSection(sctx)
	prompt += executionContextPromptSection(sctx.WorkDir)
	prompt = testguidance.LateRepairPrompt(string(types.StepRebase), prompt)

	_, err = sctx.RunAgentContext(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: commitSummarySchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		_, _ = git.Run(ctx, sctx.WorkDir, "rebase", "--abort")
		return fmt.Errorf("agent resolve conflicts: %w", err)
	}

	// Verify rebase completed (no rebase still in progress)
	if rebaseInProgress(ctx, sctx.WorkDir) {
		_, _ = git.Run(ctx, sctx.WorkDir, "rebase", "--abort")
		return fmt.Errorf("agent did not complete the rebase")
	}

	return nil
}

// mergesMovedBase reports whether this run integrates a moved base with a merge
// commit instead of rebasing onto it. The value is resolved config
// (rebase.strategy), trusted-only for a repository, and defaults to rebasing.
func mergesMovedBase(sctx *pipeline.StepContext) bool {
	return sctx.Config != nil && sctx.Config.Rebase.Strategy == config.RebaseStrategyMerge
}

// integrationVerb names the shape in log lines and findings, so a run reads as
// what it actually did rather than as the step's historical name.
func integrationVerb(merging bool) string {
	if merging {
		return "merging"
	}
	return "rebasing onto"
}

// mergeArgs builds the merge invocation. --no-ff is deliberate even though
// shouldSkipRebase already fast-forwards the strictly-behind case separately:
// the whole point of this strategy is that the integration leaves a commit with
// TWO parents behind, so whether a conflict resolution dropped content one side
// introduced stays decidable afterwards from outside the pipeline. A merge that
// silently fast-forwarded would leave nothing to compare.
// --no-edit keeps git's own "Merge remote-tracking branch 'origin/main' into
// <branch>" subject rather than inventing a second convention for the same
// commit.
func mergeArgs(targetRef string) []string {
	return []string{"merge", "--no-ff", "--no-edit", targetRef}
}

// tryMerge is merge mode's counterpart to tryRebase: it attempts to merge
// targetRef into the branch and returns the conflicted files when the merge
// stops on conflicts, aborting before returning.
func tryMerge(ctx context.Context, sctx *pipeline.StepContext, targetRef string) ([]string, error) {
	skip, err := shouldSkipRebase(ctx, sctx, targetRef)
	if err != nil {
		return nil, err
	}
	if skip {
		return nil, nil
	}

	sctx.Log(fmt.Sprintf("merging %s...", targetRef))
	if _, err := git.Run(ctx, sctx.WorkDir, mergeArgs(targetRef)...); err != nil {
		conflictFiles := rebaseConflictFiles(ctx, sctx.WorkDir)
		_, _ = git.Run(ctx, sctx.WorkDir, "merge", "--abort")

		if len(conflictFiles) == 0 {
			return nil, fmt.Errorf("merge %s: %w", targetRef, err)
		}
		return conflictFiles, nil
	}
	return nil, nil
}

// mergeWithAgent is merge mode's counterpart to rebaseWithAgent: it merges
// targetRef into the branch and hands any conflicts to the agent.
//
// The resolution prompt differs from the rebase one in the constraint it
// carries. "Minimal necessary changes" is satisfied by deleting whichever side
// is in the way, and a rebase leaves no evidence that anything was deleted. A
// merge commit does, so the prompt asks for the resolution that commit can
// actually be audited against: keep what both sides introduced.
func mergeWithAgent(ctx context.Context, sctx *pipeline.StepContext, targetRef string) error {
	skip, err := shouldSkipRebase(ctx, sctx, targetRef)
	if err != nil {
		return err
	}
	if skip {
		return nil
	}

	// Snapshot what the merge must prove before it runs. The target is pinned
	// to a SHA here rather than checked by refname afterwards because an agent
	// that fetches while resolving can advance the ref under us and fail a
	// merge that actually landed.
	preMergeHead, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("get pre-merge head: %w", err)
	}
	targetSHA, err := git.Run(ctx, sctx.WorkDir, "rev-parse", targetRef)
	if err != nil {
		return fmt.Errorf("get target head %s: %w", targetRef, err)
	}

	sctx.Log(fmt.Sprintf("merging %s...", targetRef))
	if _, err := git.Run(ctx, sctx.WorkDir, mergeArgs(targetRef)...); err == nil {
		return nil
	}

	conflictFiles := rebaseConflictFiles(ctx, sctx.WorkDir)
	if len(conflictFiles) == 0 {
		_, _ = git.Run(ctx, sctx.WorkDir, "merge", "--abort")
		return fmt.Errorf("merge %s failed (no conflicts detected)", targetRef)
	}
	sctx.Log("conflicts detected, asking agent to resolve...")

	prompt := fmt.Sprintf(
		`Resolve git merge conflicts. Merging %s into the current branch has conflicts.

Current conflicted files:
- %s

Instructions:
- Find all conflicting files and resolve the conflict markers (<<<<<<< ======= >>>>>>>).
- Resolve ADDITIVELY. Keep both sides' introduced content; never delete content one side introduced to make the merge apply. Where both sides changed the same lines, combine them so neither side's contribution is lost.
- Only where the two sides are genuinely mutually exclusive may one supersede the other, and then say which and why in the summary.
- After resolving each file, stage it with: git add <file>
- After all conflicts are resolved, conclude the merge with: git commit --no-edit
- Do not modify any files that don't have conflicts.
- Preserve the intent of both the current branch changes and the upstream changes.
- Return JSON with a single "summary" field describing what you resolved.
- Keep the summary under 10 words.`,
		targetRef,
		strings.Join(conflictFiles, "\n- "),
	)
	if sctx.PreviousFindings != "" {
		prompt += "\n\nPrevious findings:\n" + sctx.PreviousFindings
	}
	prompt += userIntentPromptSection(sctx)
	prompt += executionContextPromptSection(sctx.WorkDir)
	prompt = testguidance.LateRepairPrompt(string(types.StepRebase), prompt)

	_, err = sctx.RunAgentContext(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: commitSummarySchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		_, _ = git.Run(ctx, sctx.WorkDir, "merge", "--abort")
		return fmt.Errorf("agent resolve conflicts: %w", err)
	}

	// An unconcluded merge would leave MERGE_HEAD set and the index conflicted,
	// so the run would carry the reviewed head forward as if nothing happened.
	if mergeInProgress(ctx, sctx.WorkDir) {
		_, _ = git.Run(ctx, sctx.WorkDir, "merge", "--abort")
		return fmt.Errorf("agent did not complete the merge")
	}

	// A conflicted rebase is the other way the worktree can be left mid
	// operation, and git sets no MERGE_HEAD for it: an agent that abandons the
	// merge and rebases onto the same target hits the same conflict and stops
	// with rebase state in place. Abort it first, or the restore below would
	// move HEAD while the interrupted rebase survives underneath it.
	if rebaseInProgress(ctx, sctx.WorkDir) {
		_, _ = git.Run(ctx, sctx.WorkDir, "rebase", "--abort")
		return restorePreMergeHead(ctx, sctx, preMergeHead, fmt.Errorf("agent did not merge %s into the branch: a rebase was left in progress", targetRef))
	}

	// Concluded is not the same as merged. Requiring HEAD to have moved and to
	// carry BOTH snapshots proves a merge happened, because shouldSkipRebase
	// has already returned early unless preMergeHead and targetSHA are
	// divergent: two divergent commits can only both be ancestors of HEAD if
	// some commit in its history has two parents joining those lines. It also
	// proves the reviewed head itself was not rewritten, which target ancestry
	// alone never did and which the CI continuity rule and the attestation's
	// head binding both depend on. Every way of ending the conflict without
	// merging fails it: `git merge --abort` leaves HEAD where it was, and a
	// rebase or a `git reset --hard` onto the target drops the reviewed head
	// out of the history.
	head, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return restorePreMergeHead(ctx, sctx, preMergeHead, fmt.Errorf("get merged head: %w", err))
	}
	if head == preMergeHead {
		return restorePreMergeHead(ctx, sctx, preMergeHead, fmt.Errorf("agent did not merge %s into the branch: the branch is still at %s", targetRef, preMergeHead))
	}
	if !isAncestor(ctx, sctx.WorkDir, preMergeHead, head) {
		return restorePreMergeHead(ctx, sctx, preMergeHead, fmt.Errorf("agent did not merge %s into the branch: the reviewed head %s is not in %s", targetRef, preMergeHead, head))
	}
	if !isAncestor(ctx, sctx.WorkDir, targetSHA, head) {
		return restorePreMergeHead(ctx, sctx, preMergeHead, fmt.Errorf("agent did not merge %s into the branch: %s is not in %s", targetRef, targetSHA, head))
	}

	return nil
}

// restorePreMergeHead puts the worktree back on the reviewed head before a
// shape guard's rejection is returned. Without it a rejected merge leaves the
// branch on whatever the agent actually produced - a rebase of the reviewed
// head, a reset onto the target, an unrelated commit - and the step fails while
// the invalid head stays checked out, so any later hand-off, recovery, or
// retry reads it as the branch's real state.
//
// It is fail-closed: a restore that does not land back exactly on
// preMergeHead with a clean tree is reported as part of the returned error,
// never swallowed, so nothing is described as recovered that was not.
func restorePreMergeHead(ctx context.Context, sctx *pipeline.StepContext, preMergeHead string, cause error) error {
	if _, err := git.Run(ctx, sctx.WorkDir, "reset", "--hard", preMergeHead); err != nil {
		return fmt.Errorf("%w; restoring the branch to %s failed, the worktree is left at the rejected head: %v", cause, preMergeHead, err)
	}
	head, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("%w; restoring the branch to %s could not be verified: %v", cause, preMergeHead, err)
	}
	if head != preMergeHead {
		return fmt.Errorf("%w; restoring the branch to %s left it at %s instead", cause, preMergeHead, head)
	}
	// HEAD reading as preMergeHead is not the same as the worktree being back on
	// it: a reset performed while a rebase is interrupted moves HEAD and leaves
	// the rebase underneath it, so the restore would otherwise report a
	// recovery it never performed.
	//
	// HEAD ATTACHMENT is deliberately not verified here. The pipeline's run
	// worktree is created detached (`git worktree add --detach`) and no step
	// ever checks a branch out in it, so requiring an attached HEAD would
	// report every correct restore as a failed one. Detachment carries no
	// signal in this worktree; the reviewed-commit comparison above is what
	// proves the restore.
	if rebaseInProgress(ctx, sctx.WorkDir) {
		return fmt.Errorf("%w; restoring the branch to %s left a rebase in progress", cause, preMergeHead)
	}
	return cause
}

// shouldSkipRebase checks whether a rebase onto targetRef can be skipped.
// Returns true if targetRef doesn't exist, is already merged, or can be fast-forwarded.
func shouldSkipRebase(ctx context.Context, sctx *pipeline.StepContext, targetRef string) (bool, error) {
	if _, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", targetRef); err != nil {
		return true, nil
	}
	localSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return false, fmt.Errorf("get local head: %w", err)
	}
	targetSHA, err := git.Run(ctx, sctx.WorkDir, "rev-parse", targetRef)
	if err != nil {
		return false, fmt.Errorf("get target head %s: %w", targetRef, err)
	}
	if localSHA == targetSHA {
		sctx.Log(fmt.Sprintf("already up-to-date with %s", targetRef))
		return true, nil
	}
	if _, err := git.Run(ctx, sctx.WorkDir, "merge-base", "--is-ancestor", targetRef, "HEAD"); err == nil {
		sctx.Log(fmt.Sprintf("already ahead of %s", targetRef))
		return true, nil
	}
	if _, err := git.Run(ctx, sctx.WorkDir, "merge-base", "--is-ancestor", "HEAD", targetRef); err == nil {
		sctx.Log(fmt.Sprintf("fast-forwarding to %s", targetRef))
		if _, err := git.Run(ctx, sctx.WorkDir, "reset", "--hard", targetRef); err != nil {
			return false, fmt.Errorf("fast-forward to %s: %w", targetRef, err)
		}
		return true, nil
	}
	return false, nil
}

// rebaseInProgress returns true if a git rebase is currently in progress.
func rebaseInProgress(ctx context.Context, workDir string) bool {
	return gitPathExists(ctx, workDir, "rebase-merge", "rebase-apply")
}

// mergeInProgress returns true if a git merge is currently in progress -
// started and not yet concluded by a commit or an abort.
func mergeInProgress(ctx context.Context, workDir string) bool {
	return gitPathExists(ctx, workDir, "MERGE_HEAD")
}

// gitPathExists reports whether any of the named paths exists inside the git
// directory. It resolves them with git rev-parse --git-path, which works for
// both regular repos and worktrees (where the git dir is not .git).
func gitPathExists(ctx context.Context, workDir string, names ...string) bool {
	for _, name := range names {
		p, err := git.Run(ctx, workDir, "rev-parse", "--git-path", name)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(workDir, p)
		}
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

func rebaseConflictFiles(ctx context.Context, workDir string) []string {
	out, err := git.Run(ctx, workDir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		files = append(files, line)
	}
	return files
}

func dedupeRebaseFindings(findings []Finding) []Finding {
	if len(findings) < 2 {
		return findings
	}
	seen := make(map[string]bool, len(findings))
	filtered := make([]Finding, 0, len(findings))
	for _, finding := range findings {
		key := finding.File + "\x00" + finding.Description
		if seen[key] {
			continue
		}
		seen[key] = true
		filtered = append(filtered, finding)
	}
	return filtered
}

// updateHeadSHA syncs the run's head SHA after rebase and checks for an empty diff.
// When the branch diff against the default branch is empty, SkipRemaining is set.
func updateHeadSHA(ctx context.Context, sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve head after rebase: %w", err)
	}
	if headSHA != "" && headSHA != sctx.Run.HeadSHA {
		oldHead := sctx.Run.HeadSHA
		pipeline.RemapUncertifiedPipelineRangeAfterRebase(sctx, oldHead, headSHA)
		sctx.Run.HeadSHA = headSHA
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headSHA); err != nil {
			return nil, err
		}
		sctx.Log(fmt.Sprintf("updated head SHA to %s", shortSHA(headSHA)))
	}

	// Check if the branch has any diff against the default branch.
	// If the diff is empty (e.g. branch was already merged), skip remaining steps.
	defaultBranch := effectivePRBaseBranch(sctx)
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, defaultBranch)
	diff, err := git.Diff(ctx, sctx.WorkDir, baseSHA, "HEAD")
	if err == nil && strings.TrimSpace(diff) == "" {
		sctx.Log("empty diff after rebase, skipping remaining steps")
		return &pipeline.StepOutcome{SkipRemaining: true}, nil
	}

	return &pipeline.StepOutcome{}, nil
}

func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}
