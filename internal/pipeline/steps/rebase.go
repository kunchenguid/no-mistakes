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
	defaultBranch := strings.TrimSpace(sctx.Repo.DefaultBranch)
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	branchTarget := ""
	pushRemote := "origin"
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
	if err := git.FetchRemoteBranch(ctx, sctx.WorkDir, "origin", defaultBranch); err != nil {
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
		if pushRemote == "origin" {
			if err := git.FetchRemoteBranch(ctx, sctx.WorkDir, "origin", branch); err != nil {
				sctx.LogFile(fmt.Sprintf("warning: could not fetch origin/%s: %v", branch, err))
			}
		} else if err := git.FetchRemoteBranchToRef(ctx, sctx.WorkDir, pushRemote, branch, branchTarget); err != nil {
			sctx.LogFile(fmt.Sprintf("warning: could not fetch %s: %v", branchTarget, err))
		}
	}

	// Stop before rebasing when the gated branch carries commits that live on
	// the contributor's local default branch but were never pushed to
	// origin/<default>. Rebasing onto the fresh remote default keeps those
	// commits in the branch's history, so the PR would silently bundle another
	// workstream's unpushed work. Surface it for a human decision instead.
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

	if sctx.Fixing {
		for _, target := range targets {
			if err := rebaseWithRepair(ctx, sctx, target); err != nil {
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
			conflictTargets = append(conflictTargets, target)
			for _, file := range conflictFiles {
				conflictFindings = append(conflictFindings, Finding{
					Severity:    "warning",
					File:        file,
					Description: fmt.Sprintf("merge conflict rebasing onto %s", target),
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
// is an ancestor of the branch HEAD, then enumerates the unpushed commits.
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
		len(commits), defaultBranch, defaultBranch, len(files), strings.Join(commits, "\n- "), defaultBranch, defaultBranch,
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
		Summary: fmt.Sprintf("branch bundles %d unpushed %s commit(s)", len(commits), defaultBranch),
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

// rebaseWithRepair rebases onto targetRef and, on conflict, routes the
// resolution through the escalating conflict-repair cascade
// (fix_balanced -> authority_strong). Each tier is a transaction over the exact
// conflicted candidate: operational failure, schema-valid incomplete success,
// or protected-topology mutation restores the same index, rebase metadata,
// worktree bytes and modes, HEAD topology, and shared refs before escalation.
// Once a tier completes the rebase, deterministic git-state checks run before
// an independent strong verifier adjudicates the sealed candidate.
func rebaseWithRepair(ctx context.Context, sctx *pipeline.StepContext, targetRef string) error {
	skip, err := shouldSkipRebase(ctx, sctx, targetRef)
	if err != nil {
		return err
	}
	if skip {
		return nil
	}
	completedHeadRef, err := captureRebaseHeadRef(ctx, sctx.WorkDir)
	if err != nil {
		return err
	}

	maxTier := conflictRepairMaxTier(sctx)
	var lastErr error
	for tier := 0; tier <= maxTier; tier++ {
		sctx.Log(fmt.Sprintf("rebasing onto %s...", targetRef))
		if _, err := git.Run(ctx, sctx.WorkDir, "rebase", targetRef); err == nil {
			return nil // clean rebase, nothing to resolve
		}

		recoveryCtx := context.WithoutCancel(ctx)
		conflictFiles := rebaseConflictFiles(recoveryCtx, sctx.WorkDir)
		if len(conflictFiles) == 0 {
			_, _ = git.Run(recoveryCtx, sctx.WorkDir, "rebase", "--abort")
			return fmt.Errorf("rebase onto %s failed (no conflicts detected)", targetRef)
		}

		attempt, snapshotErr := captureRebaseAttempt(recoveryCtx, sctx.WorkDir)
		if snapshotErr != nil {
			_, _ = git.Run(recoveryCtx, sctx.WorkDir, "rebase", "--abort")
			return fmt.Errorf("snapshot conflicted rebase candidate onto %s: %w", targetRef, snapshotErr)
		}
		attempt.completedHeadRef = completedHeadRef

		sctx.Log(fmt.Sprintf("conflicts detected, resolving conflicts (tier %d)...", tier))
		_, invokeErr := sctx.InvokeAgentTier(types.PurposeUnstructuredConflictRepair, tier, agent.RunOpts{
			Prompt:           conflictRepairPrompt(sctx, targetRef, conflictFiles),
			CWD:              sctx.WorkDir,
			JSONSchema:       commitSummarySchema,
			OnChunk:          sctx.LogChunk,
			AttemptIsolation: attempt,
		})
		var profileUnavailable *agent.ProfileUnavailableError
		if errors.As(invokeErr, &profileUnavailable) {
			restoreErr := attempt.RestoreFailedAttempt()
			attempt.cleanup()
			if restoreErr != nil {
				return errors.Join(
					fmt.Errorf("conflict repair onto %s cannot continue: %w", targetRef, invokeErr),
					fmt.Errorf("restore conflicted candidate after unavailable profile: %w", restoreErr),
				)
			}
			return fmt.Errorf("conflict repair onto %s cannot continue: %w", targetRef, invokeErr)
		}

		if invokeErr == nil {
			if validationErr := attempt.ValidateSuccessfulAttempt(); validationErr != nil {
				invokeErr = fmt.Errorf("validate successful conflict-repair attempt: %w", validationErr)
			}
		}

		// A schema-valid response is still incomplete until git has finished the
		// rebase and cleared every unmerged index entry.
		if invokeErr != nil || rebaseInProgress(attempt.restoreContext, sctx.WorkDir) || len(rebaseConflictFiles(attempt.restoreContext, sctx.WorkDir)) > 0 {
			lastErr = invokeErr
			if lastErr == nil {
				lastErr = fmt.Errorf("conflict repair onto %s did not complete the rebase", targetRef)
			}
			restoreErr := attempt.RestoreFailedAttempt()
			attempt.cleanup()
			if restoreErr != nil {
				return errors.Join(lastErr, fmt.Errorf("restore conflicted candidate before escalation: %w", restoreErr))
			}
			continue
		}

		if topologyErr := attempt.validateCompletedTopology(); topologyErr != nil {
			lastErr = topologyErr
			restoreErr := attempt.RestoreFailedAttempt()
			attempt.cleanup()
			if restoreErr != nil {
				return errors.Join(topologyErr, fmt.Errorf("restore topology-mutated conflicted candidate: %w", restoreErr))
			}
			continue
		}
		attempt.cleanup()

		// Independent strong verification before the resolution is accepted.
		// The branch/history is recorded only after this passes (updateHeadSHA
		// runs in the caller once every target resolves).
		if verifyErr := verifyConflictResolution(ctx, sctx, targetRef); verifyErr != nil {
			var integrityErr *rebaseVerifierIntegrityError
			if !errors.As(verifyErr, &integrityErr) {
				// The verifier rejected an otherwise pure fixer candidate.
				// Unwind that completed rebase to its pre-rebase commit.
				if resetErr := unwindRejectedRebase(ctx, sctx.WorkDir); resetErr != nil {
					verifyErr = errors.Join(verifyErr, resetErr)
				}
			}
			return fmt.Errorf("conflict resolution onto %s failed verification: %w", targetRef, verifyErr)
		}
		return nil
	}

	return fmt.Errorf("conflict repair onto %s exhausted the fix_balanced->authority_strong cascade: %w", targetRef, lastErr)
}

func unwindRejectedRebase(ctx context.Context, workDir string) error {
	if _, err := git.Run(context.WithoutCancel(ctx), workDir, "reset", "--hard", "ORIG_HEAD"); err != nil {
		return fmt.Errorf("unwind rejected rebase candidate: %w", err)
	}
	return nil
}

// conflictRepairMaxTier is the top tier of the conflict-repair route, so the
// resolution can escalate from fix_balanced to authority_strong. It degrades to
// tier 0 (single invocation) when routing is disabled or unresolved.
func conflictRepairMaxTier(sctx *pipeline.StepContext) int {
	if sctx.Config == nil {
		return 0
	}
	profiles, err := sctx.Config.Routing.ResolveRoute(types.PurposeUnstructuredConflictRepair)
	if err != nil || len(profiles) == 0 {
		return 0
	}
	return len(profiles) - 1
}

// conflictRepairPrompt builds the fixer prompt for resolving a live rebase.
func conflictRepairPrompt(sctx *pipeline.StepContext, targetRef string, conflictFiles []string) string {
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
	return prompt
}

// verifyConflictResolution runs an independent strong verifier over a completed
// conflict resolution. The verifier receives an exact candidate transaction so
// even a legacy write-capable adapter cannot leave mutations behind. Any
// blocking finding, unparseable verdict, invocation error, or purity violation
// is inconclusive and fails closed.
func verifyConflictResolution(ctx context.Context, sctx *pipeline.StepContext, targetRef string) error {
	prompt := fmt.Sprintf(
		`You are independently verifying a completed merge-conflict resolution. The current branch was rebased onto %s and another agent resolved the conflicts.

Verify the resolution:
- Confirm no conflict markers (<<<<<<< ======= >>>>>>>) remain in any file.
- Confirm the resolution preserves the intent of BOTH the current branch changes and the upstream changes, and that no code was silently dropped.
- Confirm the result is internally coherent.

Return structured findings. Use severity "error" for any incorrect, unresolved, or dropped resolution, and return an empty findings list only when the resolution is fully correct.`,
		targetRef,
	)
	prompt += userIntentPromptSection(sctx)

	candidate, err := captureRebaseAttempt(context.WithoutCancel(ctx), sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("snapshot completed rebase candidate: %w", err)
	}
	defer candidate.cleanup()

	res, invokeErr := sctx.InvokeAgent(types.PurposeEscalatedAggregateVerification, agent.RunOpts{
		Prompt:           prompt,
		CWD:              sctx.WorkDir,
		JSONSchema:       findingsSchema,
		OnChunk:          sctx.LogChunk,
		AttemptIsolation: candidate,
	})
	purityErr := candidate.integrityError()
	currentValidationErr := candidate.validate()
	if purityErr == nil {
		purityErr = currentValidationErr
	}
	if purityErr != nil {
		if currentValidationErr != nil {
			if restoreErr := candidate.RestoreFailedAttempt(); restoreErr != nil {
				purityErr = errors.Join(purityErr, fmt.Errorf("restore sealed rebase candidate: %w", restoreErr))
			}
		}
		return &rebaseVerifierIntegrityError{cause: purityErr}
	}
	if invokeErr != nil {
		return fmt.Errorf("verifier inconclusive: %w", invokeErr)
	}
	findings, err := validateFindingsOutput(res.Output)
	if err != nil {
		return fmt.Errorf("verifier returned incomplete output: %w", err)
	}
	if len(findings.Items) > 0 {
		return fmt.Errorf("verifier rejected the resolution: %s", findings.Summary)
	}
	return nil
}

type rebaseVerifierIntegrityError struct {
	cause error
}

func (e *rebaseVerifierIntegrityError) Error() string {
	if e.cause != nil {
		return "verifier may have mutated the completed rebase candidate: " + e.cause.Error()
	}
	return "verifier mutated the completed rebase candidate"
}

func (e *rebaseVerifierIntegrityError) Unwrap() error {
	return e.cause
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

// updateHeadSHA syncs the run's head SHA after rebase and checks for an empty diff.
// When the branch diff against the default branch is empty, SkipRemaining is set.
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

	// Check if the branch has any diff against the default branch.
	// If the diff is empty (e.g. branch was already merged), skip remaining steps.
	defaultBranch := strings.TrimSpace(sctx.Repo.DefaultBranch)
	if defaultBranch == "" {
		defaultBranch = "main"
	}
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
