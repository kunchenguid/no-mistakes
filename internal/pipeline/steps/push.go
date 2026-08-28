package steps

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PushStep force-pushes the worktree state to the configured push remote.
type PushStep struct{}

func (s *PushStep) Name() types.StepName { return types.StepPush }

func (s *PushStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	ctx := sctx.Ctx
	newHeadSHA := ""
	if err := sctx.DB.SetRunPushActive(sctx.Run.ID, true); err != nil {
		return nil, err
	}
	defer func() { _ = sctx.DB.SetRunPushActive(sctx.Run.ID, false) }()

	// Run format command if configured (before committing, so changes are formatted)
	if fmtCmd := sctx.Config.Commands.Format; fmtCmd != "" {
		sctx.Log(fmt.Sprintf("running formatter: %s", fmtCmd))
		output, exitCode, err := runStepShellCommand(sctx, fmtCmd)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: format command failed: %v", err))
		} else if exitCode != 0 {
			sctx.Log(fmt.Sprintf("warning: format command exited with code %d: %s", exitCode, output))
		}
	}

	// Commit any uncommitted changes from pipeline agents or the formatter. Test
	// evidence is deliberately not among them: it is collected outside the
	// worktree and published to the orphan evidence branch (internal/evidence),
	// so no artifact ever enters the pushed branch or the default branch's history.
	status, _ := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		sctx.Log("committing agent changes...")
		if _, err := git.Run(ctx, sctx.WorkDir, "add", "-A"); err != nil {
			return nil, fmt.Errorf("stage agent changes: %w", err)
		}
		if err := commitPipelineCorrection(ctx, sctx.WorkDir, "no-mistakes: apply agent fixes", sctx.Log); err != nil {
			return nil, fmt.Errorf("commit agent changes: %w", err)
		}
		headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
		if err != nil {
			return nil, fmt.Errorf("resolve head after commit: %w", err)
		}
		newHeadSHA = headSHA
	}

	headBeingPushed, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve head before push: %w", err)
	}
	if err := publishRunHead(sctx, headBeingPushed, newHeadSHA, publishContinuity{}); err != nil {
		return nil, err
	}

	sctx.Log("pushed successfully")
	return &pipeline.StepOutcome{}, nil
}

// publishRunHead is the single guarded publication path for a run's head. Both
// the Push step and a CI repair published without revalidation
// (ci.revalidate_repairs: false) go through it, so the review-approved-head
// continuity check, the force-with-lease anchor, the remote verification, the
// push binding, and the gate-mirror update are written once and can never
// drift apart between the two callers.
//
// localRefUpdate, when non-empty, is the SHA the run's local branch ref is
// moved to after a verified push. Callers that already advanced the ref with
// their commit pass "".
//
// Every worktree git call here is step-scoped (stepGitRun), not git.Run,
// because the CI step runs with a step-local PATH and credential environment
// that a plain runner would not see. Gate-mirror calls stay on git.Run: they
// operate on the bare gate directory, not the run worktree.
type publishContinuity struct {
	ciRepair    bool
	rewriteBase string
}

func publishRunHead(sctx *pipeline.StepContext, headBeingPushed, localRefUpdate string, continuity publishContinuity) error {
	ctx := sctx.Ctx
	ref := normalizedBranchRef(sctx.Run.Branch)
	branch := strings.TrimPrefix(ref, "refs/heads/")

	pushURL := resolvePushURL(sctx)
	pushTarget := "upstream"
	usingFork := strings.TrimSpace(sctx.Repo.ForkURL) != ""
	if usingFork {
		pushTarget = "fork"
		sctx.Log(fmt.Sprintf("pushing to fork %s (%s)...", safeurl.Redact(pushURL), ref))
	} else {
		sctx.Log(fmt.Sprintf("pushing to %s (%s)...", safeurl.Redact(pushURL), ref))
	}

	if err := assertReviewApprovedPushHead(sctx, headBeingPushed, continuity); err != nil {
		return err
	}

	// Decide whether force-pushing would discard commits the pipeline never saw.
	// The lease is anchored to the remote-tracking ref the rebase step freshly
	// fetched (the exact commit this branch was rebased against) or the run's
	// own recorded prior push generation, so a push that would clobber an
	// out-of-band or stale-mirror commit fails loudly instead of silently dropping it.
	// A bare --force-with-lease offers no protection when pushing to a URL (no
	// remote-tracking refs), so the anchor is explicit.
	lastSeen := lastKnownBranchTip(ctx, sctx, branch, usingFork)
	gitRun := func(args ...string) (string, error) { return stepGitRun(sctx, args...) }
	decision, err := resolveForcePushDecision(gitRun, pushURL, ref, headBeingPushed, lastSeen, sctx.Run.BaseSHA)
	if err != nil {
		return fmt.Errorf("push to %s: %w", pushTarget, err)
	}
	switch {
	case decision.newBranch:
		// New branch: regular push (no force needed).
		if err := stepGitPushCommit(sctx, pushURL, headBeingPushed, ref, "", false); err != nil {
			return fmt.Errorf("push to %s: %w", pushTarget, err)
		}
	case decision.upToDate:
		// Remote already at this exact head. This freshly verified equality is a
		// successful binding even though no objects needed to move.
	default:
		// Existing branch: force-with-lease anchored to the verified remote head.
		if err := stepGitPushCommit(sctx, pushURL, headBeingPushed, ref, decision.remoteSHA, true); err != nil {
			return fmt.Errorf("push to %s: %w", pushTarget, err)
		}
	}
	verifiedRemote, err := lsRemoteSHA(gitRun, pushURL, ref)
	if err != nil || verifiedRemote != headBeingPushed {
		if err != nil {
			return fmt.Errorf("verify successful push to %s: %w", pushTarget, err)
		}
		return fmt.Errorf("verify successful push to %s: remote head %s does not equal pushed head %s", pushTarget, verifiedRemote, headBeingPushed)
	}
	if err := sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{
		HeadSHA:           headBeingPushed,
		TargetKind:        pushTarget,
		TargetFingerprint: branchsync.TargetFingerprint(pushURL),
		Ref:               ref,
	}); err != nil {
		return err
	}

	if localRefUpdate != "" {
		if _, err := stepGitRun(sctx, "update-ref", ref, localRefUpdate); err != nil {
			return fmt.Errorf("update local branch ref: %w", err)
		}
	}

	// Persist the immutable source that was verified and delivered, never a
	// fresh read of mutable worktree HEAD after the push.
	if headBeingPushed != sctx.Run.HeadSHA {
		sctx.Run.HeadSHA = headBeingPushed
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headBeingPushed); err != nil {
			return err
		}
	}

	// Update the gate mirror's ref so follow-up pushes to the gate proxy
	// remain fast-forwardable after pipeline rebases. At this point the remote
	// push and its durable binding have already succeeded. A Push-step retry can
	// retry a mirror failure, but the CI monitor must not call an already
	// published repair failed and spend another fix attempt on it.
	if err := updateGateMirrorAfterPush(ctx, sctx, ref, headBeingPushed); err != nil {
		if continuity.ciRepair {
			sctx.Log(fmt.Sprintf("warning: CI repair was published, but gate mirror synchronization failed: %v", err))
			return nil
		}
		return err
	}
	return nil
}

func updateGateMirrorAfterPush(ctx context.Context, sctx *pipeline.StepContext, ref, headBeingPushed string) error {
	if sctx.Repo == nil || strings.TrimSpace(sctx.GateDir) == "" {
		return nil
	}
	gateDir := strings.TrimSpace(sctx.GateDir)
	if _, statErr := os.Stat(gateDir); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil
		}
		return fmt.Errorf("stat gate mirror repository: %w", statErr)
	}
	if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
		return fmt.Errorf("update gate mirror ref %s: validate repository: %w", ref, err)
	}

	if fetchErr := git.FetchRemoteRef(ctx, gateDir, sctx.WorkDir, headBeingPushed, headBeingPushed); fetchErr != nil {
		return fmt.Errorf("update gate mirror ref %s: fetch pushed head: %w", ref, fetchErr)
	}

	gateTip, _ := git.Run(ctx, gateDir, "rev-parse", "--verify", ref)
	gateTip = strings.TrimSpace(gateTip)

	submittedHead := ""
	if sctx.Run.SubmittedHeadSHA != nil {
		submittedHead = strings.TrimSpace(*sctx.Run.SubmittedHeadSHA)
	}

	shouldUpdate := gateTip == "" || gateTip == headBeingPushed || (submittedHead != "" && gateTip == submittedHead)
	if !shouldUpdate {
		if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", headBeingPushed, gateTip); err == nil {
			// Preserve a newer descendant.
			shouldUpdate = false
		} else if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", gateTip, headBeingPushed); err == nil {
			// Fast-forward advance from an older ancestor.
			shouldUpdate = true
		} else {
			return fmt.Errorf("gate mirror ref %s at %s diverged from pushed head %s", ref, gateTip, headBeingPushed)
		}
	}
	if shouldUpdate {
		if _, updateErr := git.Run(ctx, gateDir, "update-ref", ref, headBeingPushed, gateTip); updateErr != nil {
			return fmt.Errorf("update gate mirror ref %s to %s: %w", ref, headBeingPushed, updateErr)
		}
	}
	return nil
}

func assertReviewApprovedPushHead(sctx *pipeline.StepContext, proposedHead string, continuity publishContinuity) error {
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		return fmt.Errorf("load durable review approval before push: %w", err)
	}
	if run == nil || run.ReviewApprovedHeadSHA == nil || strings.TrimSpace(*run.ReviewApprovedHeadSHA) == "" {
		return fmt.Errorf("refusing to push: run has no durably recorded review-approved head")
	}
	approvedHead := strings.TrimSpace(*run.ReviewApprovedHeadSHA)
	if !isFullGitObjectID(approvedHead) {
		return fmt.Errorf("refusing to push: durable review-approved head is malformed")
	}
	resolved, err := stepGitRun(sctx, "rev-parse", "--verify", approvedHead+"^{commit}")
	if err != nil || !strings.EqualFold(strings.TrimSpace(resolved), approvedHead) {
		return fmt.Errorf("refusing to push: durable review-approved head is unreachable")
	}
	if proposedHead == approvedHead {
		return nil
	}
	if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", approvedHead, proposedHead); err == nil {
		return nil
	}
	if continuity.ciRepair && ciRepairHeadContinuity(sctx, run, proposedHead, continuity.rewriteBase) {
		return nil
	}
	return fmt.Errorf("refusing to push: proposed head %s violates continuity with review-approved head %s (it is not an equal or descendant commit)", shortObjectID(proposedHead), shortObjectID(approvedHead))
}

func ciRepairHeadContinuity(sctx *pipeline.StepContext, run *db.Run, proposedHead, rewriteBase string) bool {
	recordedHead := strings.TrimSpace(run.HeadSHA)
	if recordedHead == "" || recordedHead != strings.TrimSpace(sctx.Run.HeadSHA) || run.LastPushedSHA == nil || strings.TrimSpace(*run.LastPushedSHA) != recordedHead {
		return false
	}
	if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", recordedHead, proposedHead); err == nil {
		return true
	}
	if rewriteBase == "" || !isFullGitObjectID(rewriteBase) {
		return false
	}
	resolved, err := stepGitRun(sctx, "rev-parse", "--verify", rewriteBase+"^{commit}")
	if err != nil || !strings.EqualFold(strings.TrimSpace(resolved), rewriteBase) {
		return false
	}
	_, err = stepGitRun(sctx, "merge-base", "--is-ancestor", rewriteBase, proposedHead)
	return err == nil
}

func isFullGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func shortObjectID(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

// lastKnownBranchTip returns the commit SHA the pipeline last observed or
// produced for this branch on the remote. It checks the current run's recorded
// pushed head, then prior pipeline runs for the same repo and branch, and
// finally falls back to the worktree's remote-tracking ref.
func lastKnownBranchTip(ctx context.Context, sctx *pipeline.StepContext, branch string, fork bool) string {
	if sctx.Run != nil && sctx.Run.LastPushedSHA != nil && strings.TrimSpace(*sctx.Run.LastPushedSHA) != "" {
		return strings.TrimSpace(*sctx.Run.LastPushedSHA)
	}
	if sctx.DB != nil && sctx.Repo != nil {
		runs, err := sctx.DB.GetRunsByRepo(sctx.Repo.ID)
		if err == nil {
			for _, r := range runs {
				if strings.TrimPrefix(r.Branch, "refs/heads/") == strings.TrimPrefix(branch, "refs/heads/") && r.LastPushedSHA != nil && strings.TrimSpace(*r.LastPushedSHA) != "" {
					return strings.TrimSpace(*r.LastPushedSHA)
				}
			}
		}
	}
	return lastFetchedBranchTip(ctx, sctx.WorkDir, branch, fork)
}
