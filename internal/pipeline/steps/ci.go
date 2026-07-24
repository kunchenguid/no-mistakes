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
	defaultChecksGracePeriod          = 60 * time.Second
	defaultBaseBranchTipResolveWindow = 30 * time.Second
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
type CIStep struct {
	lastFixedChecks      string               // sorted check names from last fix attempt, to avoid re-fixing
	lastFixedCompletedAt map[string]time.Time // failing check completion times seen before the last fix attempt
	ciFixAttempts        int                  // number of CI auto-fix attempts made
	checksGracePeriod    time.Duration        // minimum wait before trusting empty CI checks (0 = default 60s)
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

type prHeadResolver interface {
	GetPRHeadSHA(context.Context, *scm.PR) (string, error)
}

type requiredCheckResult struct {
	Pending bool
	Failing []string
}

// evaluateRequiredChecks is deliberately fail-closed. Every configured name
// must be present on the exact published head; skipped and cancelled checks
// are terminal failures rather than successful completion.
func evaluateRequiredChecks(checks []scm.Check, required []string, expectedHead, observedHead string) (requiredCheckResult, error) {
	if strings.TrimSpace(expectedHead) == "" || strings.TrimSpace(observedHead) == "" || !strings.EqualFold(strings.TrimSpace(expectedHead), strings.TrimSpace(observedHead)) {
		return requiredCheckResult{}, fmt.Errorf("required CI checks belong to PR head %q, not the exact published revision %q", observedHead, expectedHead)
	}
	byName := make(map[string][]scm.Check, len(checks))
	for _, check := range checks {
		key := strings.ToLower(strings.TrimSpace(check.Name))
		byName[key] = append(byName[key], check)
	}
	result := requiredCheckResult{}
	for _, requiredName := range required {
		matches := byName[strings.ToLower(strings.TrimSpace(requiredName))]
		if len(matches) == 0 {
			result.Pending = true // may not have registered yet; timeout still fails closed
			continue
		}
		for _, check := range matches {
			switch check.Bucket {
			case scm.CheckBucketPass:
			case scm.CheckBucketPending:
				result.Pending = true
			default: // fail, skip, cancel, and unknown terminal buckets
				result.Failing = append(result.Failing, requiredName)
			}
		}
	}
	return result, nil
}

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
	provider := scm.DetectProviderContext(sctx.Ctx, sctx.Repo.UpstreamURL)
	if provider == scm.ProviderUnknown && sctx.Run.PRURL != nil {
		provider = scm.DetectProviderContext(sctx.Ctx, *sctx.Run.PRURL)
	}
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
		if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, "merged"); err != nil {
			return false, err
		}
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

func (s *CIStep) gracePeriod() time.Duration {
	if s.checksGracePeriod > 0 {
		return s.checksGracePeriod
	}
	return defaultChecksGracePeriod
}

func (s *CIStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	ctx := sctx.Ctx
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	provider := scm.DetectProviderContext(ctx, sctx.Repo.UpstreamURL)
	if provider == scm.ProviderUnknown && sctx.Run.PRURL != nil {
		provider = scm.DetectProviderContext(ctx, *sctx.Run.PRURL)
	}
	if sctx.Config.Certification.IsCIAuthoritative() && provider != scm.ProviderGitHub {
		return nil, fmt.Errorf("ci_authoritative certification requires GitHub Actions; detected provider %q", provider)
	}
	host, skipReason := buildHost(sctx, provider)
	if host == nil {
		if sctx.Config.Certification.IsCIAuthoritative() {
			return nil, fmt.Errorf("cannot run authoritative CI certification: %s", skipReason)
		}
		sctx.Log(fmt.Sprintf("skipping CI: %s", skipReason))
		return &pipeline.StepOutcome{Skipped: true}, nil
	}
	if err := host.Available(ctx); err != nil {
		if sctx.Config.Certification.IsCIAuthoritative() {
			return nil, fmt.Errorf("cannot run authoritative CI certification: %w", err)
		}
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
		if sctx.Config.Certification.IsCIAuthoritative() {
			return nil, fmt.Errorf("cannot run authoritative CI certification without a PR URL")
		}
		sctx.Log("no PR URL found, skipping CI")
		return &pipeline.StepOutcome{Skipped: true}, nil
	}

	prNumber, err := scm.ExtractPRNumber(prURL)
	if err != nil {
		return nil, fmt.Errorf("extract PR number: %w", err)
	}
	pr := &scm.PR{Number: prNumber, URL: prURL}

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
			return resolveDefaultBranchTip(ctx, sctx.WorkDir, sctx.Repo.UpstreamURL, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
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

		elapsed := now().Sub(started)
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
			if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, "merged"); err != nil {
				return nil, err
			}
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
		checks, err := host.GetChecks(ctx, pr)
		if err != nil {
			clearCIMonitorReady(sctx)
			lastMonitorLog = ""
			sctx.Log(fmt.Sprintf("warning: could not check CI: %v", err))
		} else {
			pending := hasPendingChecks(checks)
			failing := failingCheckNames(checks)
			if sctx.Config.Certification.IsCIAuthoritative() {
				resolver, ok := host.(prHeadResolver)
				if !ok {
					return nil, fmt.Errorf("GitHub host cannot resolve the exact PR head for ci_authoritative certification")
				}
				// Resolve after reading checks. If the PR advances during either
				// request, it cannot accidentally certify the run's older SHA.
				observedHead, headErr := resolver.GetPRHeadSHA(ctx, pr)
				if headErr != nil {
					clearCIMonitorReady(sctx)
					lastMonitorLog = ""
					sctx.Log(fmt.Sprintf("warning: could not verify CI revision: %v", headErr))
					goto waitForPoll
				}
				latestRun, runErr := sctx.DB.GetRun(sctx.Run.ID)
				if runErr != nil {
					return nil, fmt.Errorf("load published revision binding: %w", runErr)
				}
				if latestRun == nil || latestRun.LastPushedSHA == nil || strings.TrimSpace(*latestRun.LastPushedSHA) == "" {
					return nil, fmt.Errorf("ci_authoritative certification has no verified published revision binding")
				}
				required, requiredErr := evaluateRequiredChecks(checks, sctx.Config.Certification.RequiredChecks, *latestRun.LastPushedSHA, observedHead)
				if requiredErr != nil {
					return nil, requiredErr
				}
				pending = required.Pending
				failing = required.Failing
			}
			sort.Strings(failing)
			hasFailures := len(failing) > 0
			hasIssues := hasFailures || mergeConflict
			timeoutFailingChecks = append(timeoutFailingChecks[:0], failing...)

			// If a failing check completed after our last fix push, CI has
			// already re-run since we pushed (possibly too fast to observe
			// as pending between polls). Treat this as a new iteration so
			// the retry path can fire rather than looping on "fix already
			// attempted" until timeout.
			if failingCheckCompletedAfter(checks, s.lastFixedCompletedAt) {
				s.lastFixedChecks = ""
				s.lastFixedCompletedAt = nil
			}

			if hasIssues {
				if err := sctx.DB.SetRunCIReady(sctx.Run.ID, false); err != nil {
					return nil, err
				}
			}
			if hasIssues && pending {
				lastMonitorLog = ""
				if pendingCheckMatchesLastFixed(checks, s.lastFixedChecks) {
					s.lastFixedChecks = ""
					s.lastFixedCompletedAt = nil
				}
				sctx.Log("issues detected but checks still pending, waiting for all checks to complete...")
			} else if hasIssues {
				lastMonitorLog = ""
				// All checks done, issues present - fix or report
				fixKey := encodeLastFixedChecks(failing, mergeConflict)
				fixCompletedAt := failingCheckCompletionTimes(checks)
				issueDesc := strings.Join(failing, ", ")
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
					pushed, err := s.autoFixCI(sctx, host, pr, failing, mergeConflict)
					if err != nil {
						sctx.Log(fmt.Sprintf("warning: CI manual fix failed: %v", err))
					} else if pushed || sctx.Run.HeadSHA != previousHeadSHA {
						s.lastFixedChecks = fixKey
						s.lastFixedCompletedAt = fixCompletedAt
					} else {
						sctx.Log("CI fix produced no changes, returning for manual intervention...")
						return ciFailureOutcome(failing, mergeConflict, "CI fix produced no changes - failures require manual intervention"), nil
					}
				} else if sctx.Fixing && fixKey == s.lastFixedChecks {
					sctx.Log("fix already attempted for these issues, waiting for CI re-run...")
				} else if ciFixLimit <= 0 {
					sctx.Log(fmt.Sprintf("issues detected: %s - auto-fix disabled, waiting for manual intervention...", issueDesc))
					return ciFailureOutcome(failing, mergeConflict, "CI failures require manual intervention"), nil
				} else if s.ciFixAttempts >= ciFixLimit {
					sctx.Log(fmt.Sprintf("issues detected: %s - max auto-fix attempts (%d) reached, waiting for manual intervention...", issueDesc, ciFixLimit))
					return ciFailureOutcome(failing, mergeConflict, "CI failures still present after auto-fix attempts"), nil
				} else if fixKey == s.lastFixedChecks {
					sctx.Log("fix already attempted for these issues, waiting for CI re-run...")
				} else {
					s.ciFixAttempts++
					sctx.Log(fmt.Sprintf("issues detected: %s - auto-fixing (attempt %d/%d)...", issueDesc, s.ciFixAttempts, ciFixLimit))
					previousHeadSHA := sctx.Run.HeadSHA
					pushed, err := s.autoFixCI(sctx, host, pr, failing, mergeConflict)
					if err != nil {
						sctx.Log(fmt.Sprintf("warning: CI auto-fix failed: %v", err))
					} else if pushed || sctx.Run.HeadSHA != previousHeadSHA {
						s.lastFixedChecks = fixKey
						s.lastFixedCompletedAt = fixCompletedAt
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
				case pending:
					// Checks are (re-)running with no failures yet. Surface this
					// so a PR that passed checks and starts re-running clears the
					// previous passed-checks signal instead of looking stale.
					lastMonitorLog = logCIMonitorStatus(sctx, ciChecksRunningMsg, lastMonitorLog)
				case len(checks) == 0 && elapsed < s.gracePeriod():
					clearCIMonitorReady(sctx)
					// CI checks may not be registered yet, keep polling.
					lastMonitorLog = ""
					sctx.Log("no CI checks reported yet, waiting for checks to register...")
				case len(checks) == 0:
					lastMonitorLog = logCIMonitorStatus(sctx, ciNoChecksPassedMsg, lastMonitorLog)
				default:
					lastMonitorLog = logCIMonitorStatus(sctx, ciChecksPassedMsg, lastMonitorLog)
				}
			}
		}

	waitForPoll:
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
		if err := sctx.DB.SetRunCIReady(sctx.Run.ID, ready); err != nil {
			sctx.Log(fmt.Sprintf("warning: could not persist CI readiness: %v", err))
		}
		sctx.Log(message)
	}
	return message
}

func clearCIMonitorReady(sctx *pipeline.StepContext) {
	if err := sctx.DB.SetRunCIReady(sctx.Run.ID, false); err != nil {
		sctx.Log(fmt.Sprintf("warning: could not clear CI readiness: %v", err))
	}
}
