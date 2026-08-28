package steps

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	defaultBaseBranchTipResolveWindow = 30 * time.Second
	defaultPublishedHeadResolveWindow = 30 * time.Second
)

// CI monitoring status messages. These are surfaced to the user and parsed by
// the TUI and the agent-facing axi commands to distinguish passed checks from
// checks that are still running. The canonical strings live in cimonitor so all
// producers and consumers agree on them.
const (
	ciChecksPassedMsg   = cimonitor.ChecksPassedMsg
	ciNoChecksPassedMsg = cimonitor.NoChecksPassedMsg
	ciChecksRunningMsg  = cimonitor.ChecksRunningMsg
)

// CIStep monitors an open PR until it is merged, closed, or its configured idle
// timeout elapses, auto-fixing CI failures.
//
// Empty check lists are never treated as green unless the resolved config
// carries the trusted default-branch `no_ci: true` declaration (config.Config.NoCI).
// A feature branch cannot self-declare that value. When checks exist, their
// actual states are always processed normally - even on a declared no-CI repo.
type CIStep struct {
	lastFixedChecks      string               // sorted check names from last fix attempt, to avoid re-fixing
	lastFixedCompletedAt map[string]time.Time // terminally failed check completion times seen before the last fix attempt
	ciFixAttempts        int                  // number of CI auto-fix attempts made
	transientReruns      checkRerunBudget     // per-check rerun budget spent on provider-reported transient failures
	pollIntervalOverride time.Duration        // if set, overrides computed poll interval (for testing)
	waitForNextPoll      func(context.Context, time.Duration) error
	now                  func() time.Time
	// baseBranchTip resolves the current tip SHA of the upstream default
	// branch. The bool is false when the SHA is a fallback/unknown value and
	// must not re-arm the timeout. Overridable for testing; defaults to
	// fetching the upstream default branch.
	baseBranchTip func(context.Context) (string, bool)
}

func (s *CIStep) Name() types.StepName { return types.StepCI }

// ReconcileApprovalGate re-checks the PR after the CI step has parked at an
// approval gate. A PR can be merged or closed after a timeout/failure gate was
// recorded; either terminal state supersedes the stale gate just as it does in
// the normal CI polling loop. Open, unknown, and provider-error states remain
// parked so reconciliation never guesses success.
func (s *CIStep) ReconcileApprovalGate(sctx *pipeline.StepContext) (bool, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return false, fmt.Errorf("%w: %w", pipeline.ErrFatalGateReconciliation, err)
	}
	if err := sctx.Ctx.Err(); err != nil {
		return false, err
	}
	provider := resolvedProvider(sctx)
	host, skipReason := buildHost(sctx, provider)
	if host == nil {
		return false, fmt.Errorf("cannot check PR state: %s", skipReason)
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return false, err
	}

	prURL := ""
	if sctx.Run.PRURL != nil {
		prURL = strings.TrimSpace(*sctx.Run.PRURL)
	}
	if prURL == "" {
		return false, fmt.Errorf("run has no PR URL")
	}
	prNumber, err := scm.ExtractPRNumber(prURL)
	if err != nil {
		return false, fmt.Errorf("extract PR number: %w", err)
	}
	state, err := host.GetPRState(sctx.Ctx, &scm.PR{Number: prNumber, URL: prURL})
	if err != nil {
		return false, err
	}
	switch state {
	case scm.PRStateMerged:
		if err := verifyMergedProof(sctx.Ctx, host, &scm.PR{Number: prNumber, URL: prURL}, sctx.Run.HeadSHA); err != nil {
			return false, err
		}
		if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, "merged"); err != nil {
			return false, err
		}
		notifyPRMerged(sctx)
		if sctx.Log != nil {
			sctx.Log("PR has been merged; clearing stale CI approval gate")
		}
		return true, nil
	case scm.PRStateClosed:
		if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, "closed"); err != nil {
			return false, err
		}
		if sctx.Log != nil {
			sctx.Log("PR has been closed; clearing stale CI approval gate")
		}
		return true, nil
	case scm.PRStateOpen:
		if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, "open"); err != nil {
			return false, err
		}
		return false, nil
	default:
		return false, fmt.Errorf("PR state is unresolved: %q", state)
	}
}

func verifyMergedProof(ctx context.Context, host scm.Host, pr *scm.PR, expectedHead string) error {
	if !host.Capabilities().MergedProof {
		return nil
	}
	proofHost, ok := host.(scm.MergedProofHost)
	if !ok {
		return fmt.Errorf("SCM provider advertises merged proof but does not implement it")
	}
	proof, err := proofHost.GetMergedProof(ctx, pr, expectedHead)
	if err != nil {
		return fmt.Errorf("verify merged PR proof: %w", err)
	}
	if !proof.Merged {
		return fmt.Errorf("verify merged PR proof: PR %s is not merged", pr.Number)
	}
	if proof.Number != pr.Number || proof.URL != pr.URL {
		return fmt.Errorf("verify merged PR proof: proof identifies PR %s at %q, want PR %s at %q", proof.Number, proof.URL, pr.Number, pr.URL)
	}
	if expectedHead != "" && proof.HeadSHA != expectedHead {
		return fmt.Errorf("verify merged PR proof: %w: expected %s, got %s", scm.ErrHeadChanged, expectedHead, proof.HeadSHA)
	}
	return nil
}

func (s *CIStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	if sctx.StepResultID != "" {
		stepResult, err := sctx.DB.GetStepResult(sctx.StepResultID)
		if err != nil {
			return nil, fmt.Errorf("restore CI auto-fix attempts: %w", err)
		}
		if stepResult != nil {
			s.ciFixAttempts = max(s.ciFixAttempts, stepResult.CIFixAttempts)
		}
	}
	// A run recovered after a restart resumes the rerun budget it already
	// spent. Without this the fresh in-memory budget would grant reruns the
	// documented limit already accounted for.
	s.loadRerunBudget(sctx)
	ctx := sctx.Ctx
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	provider := resolvedProvider(sctx)
	host, skipReason := buildHost(sctx, provider)
	if host == nil {
		sctx.Log(fmt.Sprintf("skipping CI: %s", skipReason))
		return &pipeline.StepOutcome{Skipped: true}, nil
	}
	if err := host.Available(ctx); err != nil {
		sctx.Log(fmt.Sprintf("skipping CI: %v", err))
		return &pipeline.StepOutcome{Skipped: true}, nil
	}

	// Get PR URL from run record
	prURL := ""
	if sctx.Run.PRURL != nil {
		prURL = *sctx.Run.PRURL
	}
	if prURL == "" {
		// Try to refresh from DB in case PR step set it
		run, _ := sctx.DB.GetRun(sctx.Run.ID)
		if run != nil && run.PRURL != nil {
			prURL = *run.PRURL
			sctx.Run.PRURL = run.PRURL
		}
	}
	if prURL == "" {
		sctx.Log("no PR URL found, skipping CI")
		return &pipeline.StepOutcome{Skipped: true}, nil
	}

	prNumber, err := scm.ExtractPRNumber(prURL)
	if err != nil {
		return nil, fmt.Errorf("extract PR number: %w", err)
	}
	pr := &scm.PR{Number: prNumber, URL: prURL}
	baseBranch := effectivePRBaseBranch(sctx)
	// A resumed run may have a different trusted configuration than the run
	// that created this PR. Re-read the forge record without a base filter so
	// conflict repair and tip monitoring follow the PR's actual target.
	if reader, ok := host.(scm.PRBaseBranchReader); ok {
		if actual, readErr := reader.GetPRBaseBranch(ctx, pr); readErr == nil {
			pr.BaseBranch = actual
		}
	}
	if strings.TrimSpace(pr.BaseBranch) != "" {
		baseBranch = strings.TrimSpace(pr.BaseBranch)
	}

	// CITimeout semantics: <0 (or "unlimited" in config) means never
	// self-terminate; 0 means the value was never configured, so fall back
	// to the default; >0 is an explicit finite idle timeout.
	timeout := sctx.Config.CITimeout
	unlimited := timeout < 0
	if timeout == 0 {
		timeout = config.DefaultCITimeout
	}

	if unlimited {
		sctx.Log(fmt.Sprintf("monitoring CI for PR #%s (no timeout, until merged or closed)...", prNumber))
	} else {
		sctx.Log(fmt.Sprintf("monitoring CI for PR #%s (timeout: %s)...", prNumber, timeout))
	}
	now := s.now
	if now == nil {
		now = time.Now
	}
	baseBranchTip := s.baseBranchTip
	if baseBranchTip == nil {
		baseBranchTip = func(ctx context.Context) (string, bool) {
			return resolveRunDefaultBranchTip(ctx, sctx, sctx.Run.BaseSHA, baseBranch)
		}
	}
	started := now()
	// timeoutAnchor is the point the idle timeout is measured from. It re-arms
	// to now() whenever the base branch advances, while started stays fixed so
	// poll-interval and grace-period pacing are unaffected by re-arming.
	timeoutAnchor := started
	lastBaseTip := ""
	manualFixAttempted := false
	mergeabilityBlockedReason := ""
	timeoutFailingChecks := []string{}
	timeoutMergeConflict := false
	lastMonitorLog := ""
	consecutiveCheckErrs := 0
	timeoutOutcome := func() (*pipeline.StepOutcome, error) {
		sctx.Log("CI timeout reached")
		if len(timeoutFailingChecks) > 0 || timeoutMergeConflict {
			return ciFailureOutcome(timeoutFailingChecks, timeoutMergeConflict, "CI timed out with known failures still present"), nil
		}
		if mergeabilityBlockedReason != "" {
			return ciMergeabilityOutcome("mergeability check timed out", mergeabilityBlockedReason), nil
		}
		return ciMonitoringTimeoutOutcome(), nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if !unlimited && now().Sub(timeoutAnchor) >= timeout {
			return timeoutOutcome()
		}

		// Re-arm the timeout whenever the base branch advances.
		if !unlimited {
			resolveWindow := defaultBaseBranchTipResolveWindow
			if remaining := timeout - now().Sub(timeoutAnchor); remaining <= 0 {
				return timeoutOutcome()
			} else if remaining < resolveWindow {
				resolveWindow = remaining
			}
			tipCtx, cancel := context.WithTimeout(ctx, resolveWindow)
			tip, resolved := baseBranchTip(tipCtx)
			cancel()
			if resolved && tip != "" {
				if lastBaseTip == "" {
					lastBaseTip = tip
				} else if tip != lastBaseTip {
					sctx.Log(fmt.Sprintf("base branch advanced (%s..%s), re-arming CI monitor timeout", shortSHA(lastBaseTip), shortSHA(tip)))
					timeoutAnchor = now()
					lastBaseTip = tip
				}
			}
		}

		if !unlimited && now().Sub(timeoutAnchor) >= timeout {
			return timeoutOutcome()
		}

		// Check PR state (merged/closed -> exit)
		prStateKnown := true
		state, err := host.GetPRState(ctx, pr)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: could not check PR state: %v", err))
			prStateKnown = false
		} else if state == scm.PRStateMerged {
			if err := verifyMergedProof(ctx, host, pr, sctx.Run.HeadSHA); err != nil {
				return nil, err
			}
			if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, "merged"); err != nil {
				return nil, err
			}
			notifyPRMerged(sctx)
			sctx.Log("PR has been merged!")
			return &pipeline.StepOutcome{}, nil
		} else if state == scm.PRStateClosed {
			if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, "closed"); err != nil {
				return nil, err
			}
			sctx.Log("PR has been closed")
			return &pipeline.StepOutcome{}, nil
		} else if state == scm.PRStateOpen {
			if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, "open"); err != nil {
				return nil, err
			}
		}

		// Check mergeable state if the provider supports it
		mergeConflict := false
		mergeabilityKnown := true
		if host.Capabilities().MergeableState {
			mergeState, mergeErr := host.GetMergeableState(ctx, pr)
			if mergeErr != nil {
				sctx.Log(fmt.Sprintf("warning: could not check mergeable state: %v", mergeErr))
				mergeabilityBlockedReason = ""
				mergeabilityKnown = false
			} else {
				mergeConflict = mergeState.Conflict()
				mergeabilityKnown = mergeState.Resolved()
				if !mergeabilityKnown {
					sctx.Log(fmt.Sprintf("mergeable state still pending: %s", mergeState))
					mergeabilityBlockedReason = fmt.Sprintf("PR mergeability remained unresolved before timeout: %s", mergeState)
				} else {
					mergeabilityBlockedReason = ""
					timeoutMergeConflict = mergeConflict
				}
			}
		}

		// Check CI status - wait for all checks to complete before fixing
		ciFixLimit := sctx.Config.AutoFix.CI
		pr.HeadSHA = sctx.Run.HeadSHA
		checks, err := host.GetChecks(ctx, pr)
		if err != nil {
			clearCIMonitorReady(sctx)
			lastMonitorLog = ""
			sctx.Log(fmt.Sprintf("warning: could not check CI: %v", err))
			consecutiveCheckErrs++
			// A provider read that keeps failing (e.g. gh < v2.50 rejecting
			// `gh pr checks --json`) must become an actionable stop, not an
			// invisible spin to ci_timeout. The PR state check above returned
			// already for merged/closed, so reaching here means the PR is open.
			if consecutiveCheckErrs >= consecutiveCheckErrorLimit {
				sctx.Log(fmt.Sprintf("CI checks could not be read %d consecutive times, parking for a decision", consecutiveCheckErrs))
				return ciCheckReadFailureOutcome(err), nil
			}
		} else {
			consecutiveCheckErrs = 0
			// A failure the provider produced before the repository's own steps
			// ran (a setup/action-resolution outage) is infrastructure, not a
			// verdict on the code. Re-bucket those into the transient path before
			// anything reads the failing set, so they are re-run rather than sent
			// to the fix agent. Gated on the transient budget, so an opted-out
			// repo pays no extra provider calls and keeps the prior behavior.
			markPreRunInfraFailures(sctx, host, checks)
			// checksPending is the narrow execution state: only checks that are
			// actively running or queued block a rerun or issue escalation. A
			// provider-cancelled check is terminal enough to enter the transient
			// rerun policy, even though it is not a verdict on the code.
			checksPending := hasPendingChecks(checks)
			// readinessPending is deliberately broader: any state that is not a
			// conclusive pass, failure, or skip must keep the PR non-ready. This
			// includes cancelled and unknown provider states.
			readinessPending := checksPending || hasUnresolvedChecks(checks)
			failing := failingCheckNames(checks)

			// A rerun the provider has answered is no longer outstanding. This
			// runs before anything reads the rerun bookkeeping so a resolved
			// rerun cannot be re-opened by a later poll that no longer reports
			// the check, which would park a green head on a cancellation the
			// provider already replaced.
			if _, err := s.retireResolvedReruns(sctx, checks); err != nil {
				sctx.Log(fmt.Sprintf("warning: could not persist the retired rerun state: %v", err))
			}

			// If a terminally failed check completed after our last fix push,
			// CI has already re-run since we pushed (possibly too fast to
			// observe as pending between polls). Treat this as a new iteration
			// so the retry path can fire rather than looping on "fix already
			// attempted" until timeout.
			if terminalFailureCompletedAfter(checks, s.lastFixedCompletedAt) {
				s.lastFixedChecks = ""
				s.lastFixedCompletedAt = nil
			}

			// Before any failure reaches the fix agent, re-run the checks the
			// provider itself reported as cancelled rather than as a job
			// failure. A rerun costs another provider-side workflow run;
			// escalating one costs an agent round that can edit code which was
			// never broken.
			// Genuine failures never take this path, and a merge conflict is
			// excluded outright: no rerun can ever clear one, so it must reach
			// the fix agent on its first observation.
			rerunIssued := false
			if !checksPending && !mergeConflict {
				issued, rerunOutcome := s.rerunTransientChecks(sctx, host, pr, checks)
				if rerunOutcome != nil {
					// The published head moved, so this run never delivered the
					// commit whose checks were observed: nothing here may leave
					// a ready-to-merge signal behind on the way out.
					clearCIMonitorReady(sctx)
					return rerunOutcome, nil
				}
				rerunIssued = issued
			}
			// A cancelled check is unresolved, not green, and it is not a job
			// failure either: it reaches its own approval gate below rather
			// than the fix agent. A check whose rerun the provider has not
			// published yet is neither, so the monitor keeps waiting for it.
			var unresolvedCancelled, awaitingRerun []string
			if !rerunIssued {
				unresolvedCancelled, awaitingRerun = s.transientReruns.cancelledAfterRerun(checks)
				// A cancelled check this run never re-ran is just as unresolved,
				// and just as final: the provider published a conclusion for it,
				// and with no rerun outstanding nothing this run is waiting on
				// will ever replace it. It has to reach the same gate, or a
				// repository on the default rerun budget of 0 polls a rollup
				// that has already stopped moving until its idle timeout.
				// Checks that can still finish on their own are excluded, so a
				// cancellation observed alongside a running check keeps waiting.
				// Beyond that there is no settling window, for the same reason
				// a genuine failure gets none: a status rollup is per commit,
				// so a cancellation in it belongs to the commit under test and
				// cannot be a leftover from a head this run already replaced.
				//
				// Only the cancel bucket qualifies. A check whose state this
				// version does not recognize is not known to be terminal, so it
				// stays on the wait-then-timeout path rather than being
				// escalated as a conclusion the provider never reported.
				if !checksPending {
					unresolvedCancelled = mergeCheckNames(unresolvedCancelled, s.transientReruns.cancelledWithoutRerun(checks))
				}
			}
			sort.Strings(failing)
			sort.Strings(unresolvedCancelled)
			sort.Strings(awaitingRerun)
			hasFailures := len(failing) > 0
			hasIssues := hasFailures || mergeConflict || len(unresolvedCancelled) > 0
			// reportedIssues is what the step tells the user about; failing
			// stays the set the fix agent is asked to repair.
			reportedIssues := mergeCheckNames(failing, unresolvedCancelled)
			timeoutFailingChecks = append(timeoutFailingChecks[:0], mergeCheckNames(reportedIssues, awaitingRerun)...)

			if hasIssues || len(awaitingRerun) > 0 {
				if err := setCIMonitorReadiness(sctx, false, false); err != nil {
					return nil, err
				}
			}
			if rerunIssued || (!hasIssues && len(awaitingRerun) > 0) {
				// The re-run checks are running again for the same commit, so
				// the monitor waits rather than escalating. This also clears any
				// previous passed-checks signal, which matters for a cancelled
				// check: it never counted as a failing check, so nothing above
				// cleared it.
				lastMonitorLog = logCIMonitorStatus(sctx, ciChecksRunningMsg, lastMonitorLog)
			} else if hasIssues && checksPending {
				// Issue handling waits only for checks that can still complete on
				// their own. A cancelled check whose rerun budget is exhausted must
				// reach the approval gate instead of waiting forever.
				lastMonitorLog = ""
				if pendingCheckMatchesLastFixed(checks, s.lastFixedChecks) {
					s.lastFixedChecks = ""
					s.lastFixedCompletedAt = nil
				}
				sctx.Log("issues detected but checks still pending, waiting for all checks to complete...")
			} else if hasIssues {
				lastMonitorLog = ""
				if !hasFailures && !mergeConflict && !sctx.Fixing {
					// Every remaining issue is a transient check rather than a
					// verdict on the code. No fix can clear one,
					// so this parks for a decision instead of spending a
					// fix-agent round on a run that never tested anything. The
					// CI step's outcomes are never auto-fixable, so sctx.Fixing
					// here means the user answered that gate with "fix": that
					// deliberate override is honored rather than re-parked.
					return ciUnresolvedCancelledOutcome(unresolvedCancelled, checks, s.transientReruns.used), nil
				}
				// All checks done, issues present - fix or report.
				// The fix agent is asked to repair job failures; a check the
				// provider cancelled again is not one, so it joins the request
				// only in a round the user asked for.
				fixTargets := failing
				if sctx.Fixing {
					fixTargets = reportedIssues
				}
				fixKey := encodeLastFixedChecks(fixTargets, mergeConflict)
				fixCompletedAt := terminalFailureCompletionTimes(checks)
				issueDesc := strings.Join(fixTargets, ", ")
				if mergeConflict {
					if issueDesc != "" {
						issueDesc += " + merge conflict"
					} else {
						issueDesc = "merge conflict"
					}
				}
				if sctx.Fixing && !manualFixAttempted {
					manualFixAttempted = true
					sctx.Log(fmt.Sprintf("issues detected: %s - manual fix requested...", issueDesc))
					previousHeadSHA := sctx.Run.HeadSHA
					changed, err := s.autoFixCI(sctx, host, pr, fixTargets, mergeConflict)
					if outcome := ciFixAgentBudgetOutcome(sctx, issueDesc, err); outcome != nil {
						return outcome, nil
					}
					if err != nil {
						sctx.Log(fmt.Sprintf("warning: CI manual fix failed: %v", err))
					} else if changed || sctx.Run.HeadSHA != previousHeadSHA {
						s.lastFixedChecks = fixKey
						s.lastFixedCompletedAt = fixCompletedAt
						return &pipeline.StepOutcome{RestartFrom: types.StepReview}, nil
					} else {
						sctx.Log("CI fix produced no changes, returning for manual intervention...")
						return ciFailureOutcome(reportedIssues, mergeConflict, "CI fix produced no changes - failures require manual intervention"), nil
					}
				} else if sctx.Fixing && fixKey == s.lastFixedChecks {
					sctx.Log("fix already attempted for these issues, waiting for CI re-run...")
				} else if ciFixLimit <= 0 {
					sctx.Log(fmt.Sprintf("issues detected: %s - auto-fix disabled, waiting for manual intervention...", issueDesc))
					return ciFailureOutcome(reportedIssues, mergeConflict, "CI failures require manual intervention"), nil
				} else if s.ciFixAttempts >= ciFixLimit {
					sctx.Log(fmt.Sprintf("issues detected: %s - max auto-fix attempts (%d) reached, waiting for manual intervention...", issueDesc, ciFixLimit))
					return ciFailureOutcome(reportedIssues, mergeConflict, "CI failures still present after auto-fix attempts"), nil
				} else if fixKey == s.lastFixedChecks {
					sctx.Log("fix already attempted for these issues, waiting for CI re-run...")
				} else {
					nextAttempt := s.ciFixAttempts + 1
					if sctx.StepResultID != "" {
						if err := sctx.DB.SetCIFixAttempts(sctx.StepResultID, nextAttempt); err != nil {
							return nil, fmt.Errorf("persist CI auto-fix attempt: %w", err)
						}
					}
					s.ciFixAttempts = nextAttempt
					sctx.Log(fmt.Sprintf("issues detected: %s - auto-fixing (attempt %d/%d)...", issueDesc, s.ciFixAttempts, ciFixLimit))
					previousHeadSHA := sctx.Run.HeadSHA
					changed, err := s.autoFixCI(sctx, host, pr, fixTargets, mergeConflict)
					if outcome := ciFixAgentBudgetOutcome(sctx, issueDesc, err); outcome != nil {
						return outcome, nil
					}
					if err != nil {
						sctx.Log(fmt.Sprintf("warning: CI auto-fix failed: %v", err))
					} else if changed || sctx.Run.HeadSHA != previousHeadSHA {
						s.lastFixedChecks = fixKey
						s.lastFixedCompletedAt = fixCompletedAt
						return &pipeline.StepOutcome{RestartFrom: types.StepReview}, nil
					} else {
						// No changes produced - don't set lastFixedChecks so next
						// poll treats this as a new failure and retries if attempts remain.
						sctx.Log("CI fix produced no changes, will retry if attempts remain...")
					}
				}
			} else {
				s.lastFixedChecks = ""
				s.lastFixedCompletedAt = nil
				switch {
				case !prStateKnown || !mergeabilityKnown:
					clearCIMonitorReady(sctx)
					lastMonitorLog = ""
				case readinessPending:
					// Checks are (re-)running with no failures yet. Surface this
					// so a PR that passed checks and starts re-running clears the
					// previous passed-checks signal instead of looking stale.
					// The broader readiness state is intentional here: cancelled
					// and unknown checks must never be promoted as green.
					// Applies even when no_ci is declared: registered checks are
					// never waived.
					lastMonitorLog = logCIMonitorStatus(sctx, ciChecksRunningMsg, lastMonitorLog)
				case len(checks) == 0:
					// Empty forge results are ready ONLY with positive durable
					// evidence from trusted default-branch config (no_ci: true).
					// Without that declaration, keep waiting - delayed registration
					// is common and must never look green. Elapsed time is not
					// evidence; there is no grace-period promotion path.
					if sctx.Config != nil && sctx.Config.NoCI {
						lastMonitorLog = logCIMonitorStatus(sctx, ciNoChecksPassedMsg, lastMonitorLog)
					} else {
						clearCIMonitorReady(sctx)
						lastMonitorLog = ""
						sctx.Log("no CI checks reported yet, waiting for checks to register...")
					}
				case allChecksPassed(checks):
					lastMonitorLog = logCIMonitorStatus(sctx, ciChecksPassedMsg, lastMonitorLog)
				default:
					clearCIMonitorReady(sctx)
					lastMonitorLog = logCIMonitorStatus(sctx, ciChecksRunningMsg, lastMonitorLog)
				}
			}
		}

		// Sleep for poll interval
		interval := s.pollIntervalOverride
		if interval == 0 {
			interval = pollInterval(now().Sub(started))
		}
		if !unlimited {
			remaining := timeout - now().Sub(timeoutAnchor)
			if remaining < interval {
				interval = remaining
			}
		}
		waitForNextPoll := s.waitForNextPoll
		if waitForNextPoll == nil {
			waitForNextPoll = func(ctx context.Context, interval time.Duration) error {
				select {
				case <-time.After(interval):
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		if err := waitForNextPoll(ctx, interval); err != nil {
			return nil, err
		}
	}
}

func logCIMonitorStatus(sctx *pipeline.StepContext, message, previous string) string {
	if message != previous {
		ready := message == ciChecksPassedMsg || message == ciNoChecksPassedMsg
		declaredNoCI := message == ciNoChecksPassedMsg
		if err := setCIMonitorReadiness(sctx, ready, declaredNoCI); err != nil {
			sctx.Log(fmt.Sprintf("warning: could not persist CI readiness: %v", err))
		}
		sctx.Log(message)
	}
	return message
}

func clearCIMonitorReady(sctx *pipeline.StepContext) {
	if err := setCIMonitorReadiness(sctx, false, false); err != nil {
		sctx.Log(fmt.Sprintf("warning: could not clear CI readiness: %v", err))
	}
}

func setCIMonitorReadiness(sctx *pipeline.StepContext, ready, declaredNoCI bool) error {
	declaredNoCI = ready && declaredNoCI
	if err := sctx.DB.SetRunCIReadyWithReason(sctx.Run.ID, ready, declaredNoCI); err != nil {
		return err
	}
	if sctx.CIReadinessChanged != nil {
		sctx.CIReadinessChanged(ready, declaredNoCI)
	}
	return nil
}

func notifyPRMerged(sctx *pipeline.StepContext) {
	if sctx == nil || sctx.OnPRMerged == nil || sctx.Run == nil {
		return
	}
	sctx.OnPRMerged(sctx.Ctx, sctx.Run.ID)
}
