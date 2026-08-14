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
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testguidance"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RebaseStep syncs the pushed branch with the configured push target and the
// run's immutable parent-upstream target.
type RebaseStep struct{}

func (s *RebaseStep) Name() types.StepName { return types.StepRebase }

const forkBranchRefPrefix = "refs/remotes/no-mistakes-push/"

func (s *RebaseStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	targetBranch, targetSHA, pinnedTarget, err := runTargetIdentity(sctx)
	if err != nil {
		return nil, err
	}
	if targetBranch == "" {
		return nil, fmt.Errorf("run target branch is unavailable")
	}
	targetRef := "origin/" + targetBranch
	if pinnedTarget {
		resolved, resolveErr := git.ResolveRef(ctx, sctx.WorkDir, targetSHA)
		if resolveErr != nil || !strings.EqualFold(strings.TrimSpace(resolved), targetSHA) {
			return nil, fmt.Errorf("run target %s@%s is not readable", targetBranch, shortSHA(targetSHA))
		}
		targetRef = targetSHA
		sctx.Log(fmt.Sprintf("target branch %s pinned at %s", targetBranch, targetSHA))
	}
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
	if !pinnedTarget {
		if err := fetchRunUpstreamBranch(ctx, sctx, targetBranch); err != nil {
			sctx.LogFile(fmt.Sprintf("warning: could not fetch origin/%s: %v", targetBranch, err))
		}
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
	if !forcePush && branch != "" && branch != targetBranch {
		if strings.TrimSpace(sctx.Repo.ForkURL) == "" {
			if err := fetchRunUpstreamBranch(ctx, sctx, branch); err != nil {
				sctx.LogFile(fmt.Sprintf("warning: could not fetch origin/%s: %v", branch, err))
			}
		} else if err := git.FetchRemoteBranchToRef(ctx, sctx.WorkDir, pushRemote, branch, branchTarget); err != nil {
			sctx.LogFile(fmt.Sprintf("warning: could not fetch %s: %v", branchTarget, err))
		}
	}

	// Stop before rebasing when the gated branch carries commits that live on
	// the contributor's local target branch but were never pushed to
	// origin/<target>. Rebasing onto the pinned upstream target keeps those
	// commits in the branch's history, so the PR would silently bundle another
	// workstream's unpushed work. Surface it for a human decision instead.
	if outcome := detectBundledLocalTargetCommits(ctx, sctx, branch, targetBranch, targetRef); outcome != nil {
		return outcome, nil
	}
	if forcePush && branch == targetBranch && remoteTargetBranchAdvanced(ctx, sctx.WorkDir, targetBranch, sctx.Run.BaseSHA) {
		findingsJSON, _ := json.Marshal(Findings{
			Items: []Finding{{
				Severity:    "warning",
				File:        filepath.Join("internal", "pipeline", "steps", "rebase.go"),
				Description: fmt.Sprintf("origin/%s advanced after the force push; manual review required before updating the target branch", targetBranch),
			}},
			Summary: fmt.Sprintf("remote %s advanced during force push", targetBranch),
		})
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			Findings:      string(findingsJSON),
		}, nil
	}

	targets := rebaseTargetsForRun(branch, targetBranch, branchTarget, targetRef)
	if forcePush {
		sctx.Log("force push detected, skipping " + branchTarget + " sync")
		targets = forcePushRebaseTargetsForRun(branch, targetBranch, targetRef)
	}

	if sctx.Fixing {
		for _, target := range targets {
			if err := rebaseWithAgent(ctx, sctx, target); err != nil {
				return nil, err
			}
		}
		return updateHeadSHA(ctx, sctx)
	}

	// Normal mode: try all rebases, track which targets had conflicts
	var conflictTargets []string
	var conflictFindings []Finding
	for _, target := range targets {
		conflictFiles, err := tryRebase(ctx, sctx, target)
		if err != nil {
			return nil, err
		}
		if len(conflictFiles) > 0 {
			evidenceTarget := target
			if pinnedTarget && target == targetRef {
				evidenceTarget = fmt.Sprintf("origin/%s@%s", targetBranch, targetSHA)
			}
			conflictTargets = append(conflictTargets, evidenceTarget)
			for _, file := range conflictFiles {
				conflictFindings = append(conflictFindings, Finding{
					Severity:    "warning",
					File:        file,
					Description: fmt.Sprintf("merge conflict rebasing onto %s", evidenceTarget),
				})
			}
		}
	}

	if len(conflictTargets) > 0 {
		summary := fmt.Sprintf("conflict rebasing onto %s", strings.Join(conflictTargets, ", "))
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
	return rebaseTargetsForRun(branch, defaultBranch, branchTarget, "origin/"+defaultBranch)
}

func rebaseTargetsForRun(branch, targetBranch, branchTarget, targetRef string) []string {
	var targets []string
	if branch != "" && branch != targetBranch {
		targets = append(targets, branchTarget)
	}
	if branch != targetBranch {
		targets = append(targets, targetRef)
	}
	return targets
}

// forcePushRebaseTargets returns rebase targets for a force push. The pushed
// branch target is skipped because it may contain autofix commits from prior
// pipeline runs that the force push intended to discard.
func forcePushRebaseTargets(branch, defaultBranch string) []string {
	return forcePushRebaseTargetsForRun(branch, defaultBranch, "origin/"+defaultBranch)
}

func forcePushRebaseTargetsForRun(branch, targetBranch, targetRef string) []string {
	if branch == targetBranch {
		return nil
	}
	return []string{targetRef}
}

// detectBundledLocalTargetCommits returns a blocking finding when the gated
// branch carries commits that exist on the contributor's local target branch
// but were never pushed to origin/<target>. In multi-session / monorepo setups
// the local target branch can carry another workstream's unpushed
// work; branching a fix off that local tip silently drags it into the PR when
// the branch is rebased onto the upstream target. Returns nil when no such
// divergence is detected so the run proceeds normally.
//
// It only flags commits the branch actually carries: it reads the local target
// tip from the working repo, confirms that tip is ahead of origin/<target> and
// is an ancestor of the branch HEAD, then enumerates the unpushed commits.
// Detection is best-effort - if the local default tip advanced past the branch
// point, or the working repo cannot be read, it returns nil rather than guess.
func detectBundledLocalTargetCommits(ctx context.Context, sctx *pipeline.StepContext, branch, targetBranch, targetRef string) *pipeline.StepOutcome {
	if branch == "" || branch == targetBranch {
		return nil
	}
	workingPath := strings.TrimSpace(sctx.Repo.WorkingPath)
	if workingPath == "" {
		return nil
	}
	localTip, err := git.Run(ctx, workingPath, "rev-parse", "--verify", "--quiet", "refs/heads/"+targetBranch+"^{commit}")
	if err != nil {
		return nil
	}
	localTip = strings.TrimSpace(localTip)
	if localTip == "" {
		return nil
	}
	remoteRef := targetRef
	if _, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", "--quiet", remoteRef+"^{commit}"); err != nil {
		return nil
	}
	// The local target tip must be present in the gate's object store (it is
	// when the branch carries it as an ancestor) for the reachability checks.
	if _, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", "--quiet", localTip+"^{commit}"); err != nil {
		return nil
	}
	// Already pushed (local target not ahead of remote) -> nothing bundled.
	if isAncestor(ctx, sctx.WorkDir, localTip, remoteRef) {
		return nil
	}
	// The branch must actually carry the local target tip's commits.
	if !isAncestor(ctx, sctx.WorkDir, localTip, "HEAD") {
		return nil
	}

	subjects, err := git.Run(ctx, sctx.WorkDir, "log", "--oneline", "--no-decorate", remoteRef+".."+localTip)
	if err != nil || strings.TrimSpace(subjects) == "" {
		return nil
	}
	commits := strings.Split(strings.TrimSpace(subjects), "\n")
	files, _ := git.DiffNameOnly(ctx, sctx.WorkDir, remoteRef, localTip)
	firstFile := ""
	if len(files) > 0 {
		firstFile = files[0]
	}

	description := fmt.Sprintf(
		"branch carries %d commit(s) that exist on your local %s branch but were never pushed to origin/%s; rebasing would bundle this unrelated work (%d file(s)) into the PR:\n- %s\n\nPush %s to origin, or rebase your branch onto origin/%s, before gating.",
		len(commits), targetBranch, targetBranch, len(files), strings.Join(commits, "\n- "), targetBranch, targetBranch,
	)
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
		Summary: fmt.Sprintf("branch bundles %d unpushed %s commit(s)", len(commits), targetBranch),
	})
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		AutoFixable:   false,
		Findings:      string(findingsJSON),
	}
}

func isAncestor(ctx context.Context, workDir, ancestor, descendant string) bool {
	_, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

func remoteTargetBranchAdvanced(ctx context.Context, workDir, targetBranch, baseSHA string) bool {
	if baseSHA == "" || git.IsZeroSHA(baseSHA) {
		return false
	}
	remoteSHA, err := git.Run(ctx, workDir, "rev-parse", "--verify", "origin/"+targetBranch)
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
	prompt = testguidance.LateRepairPrompt(string(types.StepRebase), prompt)

	_, err = sctx.Agent.Run(ctx, agent.RunOpts{
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
// Uses git rev-parse --git-path which works for both regular repos and worktrees.
func rebaseInProgress(ctx context.Context, workDir string) bool {
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		p, err := git.Run(ctx, workDir, "rev-parse", "--git-path", dir)
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

// updateHeadSHA syncs the run's head SHA after rebase and checks for an empty
// diff against the run-owned target.
func updateHeadSHA(ctx context.Context, sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve head after rebase: %w", err)
	}
	if headSHA != "" && headSHA != sctx.Run.HeadSHA {
		sctx.Run.HeadSHA = headSHA
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headSHA); err != nil {
			return nil, err
		}
		sctx.Log(fmt.Sprintf("updated head SHA to %s", shortSHA(headSHA)))
	}

	// Check if the branch has any diff against the run target.
	// If the diff is empty (e.g. branch was already merged), skip remaining steps.
	baseSHA, err := resolveRunBranchBaseSHA(ctx, sctx)
	if err != nil {
		return nil, err
	}
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
