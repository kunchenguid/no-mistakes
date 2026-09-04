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
	status, err := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("check agent changes: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		sctx.Log("committing agent changes...")
		if err := stagePipelineChanges(sctx); err != nil {
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
	if err := publishRunHead(sctx, headBeingPushed, newHeadSHA); err != nil {
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
// Publication becomes durable only after the remote and gate mirror settle;
// the push binding and recorded head then land in one database update.
//
// It deliberately does not relax the review-approved-head check for anyone.
// Whether a CI repair may be published at all is decided before publication, by
// ciRepairContinuityGap.
func publishRunHead(sctx *pipeline.StepContext, headBeingPushed, localRefUpdate string) error {
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

	if err := assertReviewApprovedPushHead(sctx, headBeingPushed); err != nil {
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
	// Settle the gate mirror BEFORE recording the publication. The remote
	// already has the head, but a run is only "published" once the gate mirror
	// carries it too: `no-mistakes rerun` resolves its starting head from the
	// gate, so a head recorded as published while the gate is behind is a head
	// a later rerun silently omits.
	//
	// Ordering it here is what makes a mirror failure retryable instead of
	// having to choose between two wrong answers. Nothing durable has been
	// written yet, so the caller's next attempt re-enters this path, finds the
	// remote already at this head (an up-to-date no-op push), and retries the
	// mirror. The alternative orderings both lose: recording first and
	// returning the error makes the CI monitor treat an already published
	// repair as a failed one, and recording first and swallowing the error
	// strands the gate behind the remote for good.
	if err := updateGateMirrorAfterPush(ctx, sctx, ref, headBeingPushed); err != nil {
		return err
	}

	if localRefUpdate != "" {
		if _, err := stepGitRun(sctx, "update-ref", ref, localRefUpdate); err != nil {
			return fmt.Errorf("update local branch ref: %w", err)
		}
	}

	if err := sctx.DB.UpdateRunPublication(sctx.Run.ID, db.PushBinding{
		HeadSHA:           headBeingPushed,
		TargetKind:        pushTarget,
		TargetFingerprint: branchsync.TargetFingerprint(pushURL),
		Ref:               ref,
	}); err != nil {
		return err
	}
	sctx.Run.HeadSHA = headBeingPushed
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

// assertReviewApprovedPushHead refuses to publish a head that is not the
// durably review-approved commit or a descendant of it. There is no exception:
// a head that cannot show that ancestry has not been reviewed, and the CI
// repair path answers that case by revalidating instead of publishing.
func assertReviewApprovedPushHead(sctx *pipeline.StepContext, proposedHead string) error {
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		return fmt.Errorf("load durable review approval before push: %w", err)
	}
	approvedHead, reason := reviewApprovedHead(sctx, run)
	if approvedHead == "" {
		return fmt.Errorf("refusing to push: %s", reason)
	}
	if proposedHead == approvedHead {
		return nil
	}
	if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", approvedHead, proposedHead); err != nil {
		return fmt.Errorf("refusing to push: proposed head %s violates continuity with review-approved head %s (it is not an equal or descendant commit)", shortObjectID(proposedHead), shortObjectID(approvedHead))
	}
	return nil
}

// reviewApprovedHead returns the run's durable review-approved commit, or ""
// plus the reason it is unusable. It is the single reader of that authority, so
// the pre-publication continuity decision and the publication guard itself can
// never disagree about what "reviewed" means.
func reviewApprovedHead(sctx *pipeline.StepContext, run *db.Run) (string, string) {
	if run == nil || run.ReviewApprovedHeadSHA == nil || strings.TrimSpace(*run.ReviewApprovedHeadSHA) == "" {
		return "", "run has no durably recorded review-approved head"
	}
	approvedHead := strings.TrimSpace(*run.ReviewApprovedHeadSHA)
	if !isFullGitObjectID(approvedHead) {
		return "", "durable review-approved head is malformed"
	}
	resolved, err := stepGitRun(sctx, "rev-parse", "--verify", approvedHead+"^{commit}")
	if err != nil || !strings.EqualFold(strings.TrimSpace(resolved), approvedHead) {
		return "", "durable review-approved head is unreachable"
	}
	return approvedHead, ""
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
