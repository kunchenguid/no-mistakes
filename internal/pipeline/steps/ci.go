package steps

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const defaultBaseBranchTipResolveWindow = 30 * time.Second

// publishedHeadResolveWindow bounds each remote read of the published branch
// head. Overridable so tests can drive the deadline without waiting it out.
var publishedHeadResolveWindow = 30 * time.Second

// CI monitoring status messages. These are surfaced to the user and parsed by
// the TUI and the agent-facing axi commands to distinguish passed checks from
// checks that are still running. The canonical strings live in cimonitor so all
// producers and consumers agree on them.
const (
	ciChecksPassedMsg            = cimonitor.ChecksPassedMsg
	ciChecksPassedCompleteMsg    = cimonitor.ChecksPassedCompleteMsg
	ciNoChecksPassedMsg          = cimonitor.NoChecksPassedMsg
	ciNoChecksPassedCompleteMsg  = cimonitor.NoChecksPassedCompleteMsg
	ciChecksRunningMsg           = cimonitor.ChecksRunningMsg
	ciChecksAwaitingApprovalMsg  = cimonitor.ChecksAwaitingApprovalMsg
	ciChecksAwaitingManualJobMsg = cimonitor.ChecksAwaitingManualJobMsg
	ciBehindBaseMsg              = cimonitor.BehindBaseMsg
)

// CIStep monitors an open PR until it is merged, closed, explicitly handed off,
// or its configured idle timeout elapses, and repairs CI failures through the executor's shared
// findings machinery. With ci.release_after_checks_pass it instead finishes the
// run at the checks verdict.
//
// By default the green observation only clears the failure signal: the monitor
// keeps watching the PR, so the run stays open and a later base-branch advance
// is still repaired on its own. That is also why the recorded step duration
// includes merge-waiting time. The opt-in release finishes the step there
// instead, recording readiness and completing the run with the terminal
// checks_passed status, which releases its worktree and lane and leaves the
// merge decision to a human.
//
// Execute has the same shape as the review step's: when the executor
// re-executes it as a fix round (sctx.Fixing), it first repairs the findings
// selected for that round (sctx.PreviousFindings), then it monitors. Each
// settled observation - every check finished, every authorized transient
// rerun spent, at least one issue present - returns one finding per issue
// carrying its action (ciObservationFindings), and the executor decides what
// happens next: auto-fix findings enter the auto_fix.ci loop as fix rounds,
// ask-user findings park for a decision. The step never counts or limits fix
// rounds itself; the executor does, durably, from the round history.
//
// Empty check lists are never treated as green unless the resolved config
// carries the trusted default-branch `no_ci: true` declaration (config.Config.NoCI).
// A feature branch cannot self-declare that value. When checks exist, their
// actual states are always processed normally - even on a declared no-CI repo.
type CIStep struct {
	lastFixedChecks      string                    // encoded targets of the last published repair, so a poll that still shows them is not re-escalated
	lastFixedCompletedAt map[string]checkFreshness // terminally failed check freshness at the observation the last repair targeted
	observedCompletedAt  map[string]checkFreshness // terminally failed check freshness at the observation whose findings a fix round may repair
	pendingFixSummary    string                    // one-line summary of the repair this execution published, attached to the outcome it ends with
	pendingRepairPublish bool
	fixAgentCrashes      int
	lastFixAgentCrash    string
	transientReruns      checkRerunBudget // per-check rerun budget spent on provider-reported transient failures
	pollIntervalOverride time.Duration    // if set, overrides computed poll interval (for testing)
	waitForNextPoll      func(context.Context, time.Duration) error
	now                  func() time.Time
	// baseBranchTip resolves the current tip SHA of the upstream default
	// branch. The bool is false when the SHA is a fallback/unknown value and
	// must not re-arm the timeout. Overridable for testing; defaults to
	// fetching the upstream default branch.
	baseBranchTip func(context.Context) (string, bool)
	// replayBase is the resolved base tip an in-flight rebase repair targets,
	// set only for the duration of that repair's recording. recordRepair
	// proves a pure replay against exactly this commit and nothing else.
	replayBase string
}

// SetPollIntervalOverride is a test hook; production leaves the override unset.
func (s *CIStep) SetPollIntervalOverride(d time.Duration) *CIStep {
	s.pollIntervalOverride = d
	return s
}

// SetWaitForNextPoll is a test hook; production sleeps for the poll interval.
func (s *CIStep) SetWaitForNextPoll(fn func(context.Context, time.Duration) error) *CIStep {
	s.waitForNextPoll = fn
	return s
}

// SetNow is a test hook; production uses time.Now.
func (s *CIStep) SetNow(fn func() time.Time) *CIStep {
	s.now = fn
	return s
}

// SetBaseBranchTip is a test hook; production fetches the upstream default branch.
func (s *CIStep) SetBaseBranchTip(fn func(context.Context) (string, bool)) *CIStep {
	s.baseBranchTip = fn
	return s
}

// TransientRerunRecorded reports whether a rerun budget entry exists for name.
// Tests use this to assert monitor accounting without reading unexported state.
func (s *CIStep) TransientRerunRecorded(name string) bool {
	if s == nil {
		return false
	}
	_, ok := s.transientReruns.rollup[name]
	return ok
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
		if err := setCIMonitorPRState(sctx, "merged"); err != nil {
			return false, err
		}
		notifyPRMerged(sctx)
		if sctx.Log != nil {
			sctx.Log("PR has been merged; clearing stale CI approval gate")
		}
		return true, nil
	case scm.PRStateClosed:
		if err := setCIMonitorPRState(sctx, "closed"); err != nil {
			return false, err
		}
		if sctx.Log != nil {
			sctx.Log("PR has been closed; clearing stale CI approval gate")
		}
		return true, nil
	case scm.PRStateOpen:
		if err := setCIMonitorPRState(sctx, "open"); err != nil {
			return false, err
		}
		return false, nil
	default:
		return false, fmt.Errorf("PR state is unresolved: %q", state)
	}
}

// VerifyApprovalOverride reads live checks without fixing or rerunning them.
// Required reviews must be proven complete at the run's current head before
// approval can use the ordinary CI override path. Any earlier return leaves
// that proof absent, so the deferred guard refuses completion.
func (s *CIStep) VerifyApprovalOverride(sctx *pipeline.StepContext) (reason string, verifyErr error) {
	requiredComplete := !requiresExternalReview(sctx)
	defer func() {
		if !requiredComplete {
			if verifyErr != nil {
				verifyErr = fmt.Errorf("%w: %w", pipeline.ErrApprovalBlocked, verifyErr)
			} else {
				verifyErr = fmt.Errorf("%w: %s", pipeline.ErrApprovalBlocked, reason)
			}
		}
	}()
	ctx := sctx.Ctx
	if err := ctx.Err(); err != nil {
		return "", err
	}
	provider := resolvedProvider(sctx)
	host, skipReason := buildHost(sctx, provider)
	if host == nil {
		return fmt.Sprintf("could not verify live CI state: %s", skipReason), nil
	}
	if err := host.Available(ctx); err != nil {
		return fmt.Sprintf("could not verify live CI state: %v", err), nil
	}

	prURL := ""
	if sctx.Run.PRURL != nil {
		prURL = strings.TrimSpace(*sctx.Run.PRURL)
	}
	if prURL == "" {
		return "could not verify live CI state: run has no PR URL", nil
	}
	prNumber, err := scm.ExtractPRNumber(prURL)
	if err != nil {
		return fmt.Sprintf("could not verify live CI state: %v", err), nil
	}
	pr := &scm.PR{Number: prNumber, URL: prURL}
	// The delivered commit is named on every read, not only when review-bot
	// identity needs the head's rollup: a host that binds its reads to the
	// named commit refuses one that is not evidence for it (GitLab's head
	// pipeline can still point at an older commit), and an operator override
	// must never be recorded from another commit's checks. Review-bot identity
	// comes from that same head read, exactly as the polling loop names it.
	pr.HeadSHA = sctx.Run.HeadSHA
	checks, _, _, err := getCIChecks(ctx, host, pr)
	if err == nil {
		checks, err = requiredReviewChecks(sctx, host, pr, checks)
	}
	if err != nil {
		if errors.Is(err, scm.ErrHeadChanged) {
			// The provider answered; nothing it offered is evidence for the
			// commit this run delivered. That is not a broken tool and not a
			// clean pass: record it as an override naming the mismatch, the
			// same refusal the polling loop waits on.
			return fmt.Sprintf("live checks for %s are not for the commit being delivered: %v", prURL, err), nil
		}
		return fmt.Sprintf("could not verify live CI state: %v", err), nil
	}
	for _, check := range checks {
		if check.ExternalReview != "" && check.Bucket != scm.CheckBucketPass {
			return fmt.Sprintf("%s is %s at the current PR head", check.Name, check.State), nil
		}
	}
	requiredComplete = true
	if allChecksPassed(checks) {
		// A green head can still carry a green review bot's unresolved
		// comments under ci.review_bot_comments: always; approving over
		// them is an override, not a clean pass.
		botFindings, err := s.greenReviewBotFindings(sctx, host, pr, checks)
		if err != nil {
			return fmt.Sprintf("could not verify review bot comments: %v", err), nil
		}
		if len(botFindings.Items) > 0 {
			return fmt.Sprintf("live checks for %s passed but %s", prURL, botFindings.Summary), nil
		}
		return "", nil
	}
	if len(checks) == 0 {
		return fmt.Sprintf("live checks for %s: no checks reported", prURL), nil
	}
	return fmt.Sprintf("live checks for %s not all passed: %s", prURL, strings.Join(unresolvedCheckNames(checks), ", ")), nil
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

func (s *CIStep) Execute(sctx *pipeline.StepContext) (outcome *pipeline.StepOutcome, err error) {
	refusalFindings := ""
	if sctx.StepResultID != "" {
		stepResult, err := sctx.DB.GetStepResult(sctx.StepResultID)
		if err != nil {
			return nil, fmt.Errorf("restore CI protected-path refusal: %w", err)
		}
		if stepResult != nil && stepResult.FindingsJSON != nil {
			refusalFindings = *stepResult.FindingsJSON
		}
	}
	retryRefusal := sctx.Fixing && pipeline.HasProtectedPathRefusal(refusalFindings)
	// A fix round repairs the findings the executor selected for it, unless
	// this re-entry is the retry of a retained repair a protected-path refusal
	// interrupted: that repair is finished first and nothing new is requested.
	repairRequested := sctx.Fixing && !retryRefusal
	defer func() {
		if outcome != nil {
			if s.fixAgentCrashes > 0 {
				findings, parseErr := types.ParseFindingsJSON(outcome.Findings)
				if outcome.Findings == "" || parseErr == nil {
					// Deferred findings may already contain the previous round's
					// diagnostic. Replace it with the latest cumulative count and cause.
					items := findings.Items[:0]
					for _, item := range findings.Items {
						if item.ID != "ci-fix-agent-crashes" {
							items = append(items, item)
						}
					}
					findings.Items = items
					findings.Items = append(findings.Items, Finding{
						ID:          "ci-fix-agent-crashes",
						Severity:    types.FindingSeverityInfo,
						Action:      types.ActionNoOp,
						Description: fmt.Sprintf("%d CI fix-agent crashes; last cause: %s", s.fixAgentCrashes, s.lastFixAgentCrash),
					})
					outcome.Findings, _ = types.MarshalFindingsJSON(findings)
				}
			}
			if s.pendingFixSummary != "" {
				outcome.FixSummary = s.pendingFixSummary
			}
			outcome.RepairPublished = s.pendingRepairPublish
		}
		s.pendingFixSummary = ""
		s.pendingRepairPublish = false
	}()
	defer func() {
		if !retryRefusal {
			return
		}
		if refusal := pipeline.ProtectedPathOutcome(err); refusal != nil {
			outcome, err = refusal, nil
			return
		}
		findings, _ := types.ParseFindingsJSON(refusalFindings)
		findings.Summary = "Retained CI repair could not finish; resolve the failure and retry with fix"
		if err != nil {
			findings.Summary += ": " + safeurl.RedactText(err.Error())
		} else if outcome != nil && outcome.SkipReason != "" {
			findings.Summary += ": " + safeurl.RedactText(outcome.SkipReason)
		}
		encoded, _ := types.MarshalFindingsJSON(findings)
		outcome, err = &pipeline.StepOutcome{NeedsApproval: true, Findings: encoded}, nil
	}()
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
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
		if requiresExternalReview(sctx) {
			return ciCheckReadFailureOutcome(fmt.Errorf("required external review unavailable: %s", skipReason)), nil
		}
		sctx.Log(fmt.Sprintf("skipping CI: %s", skipReason))
		return &pipeline.StepOutcome{Skipped: true, SkipReason: skipReason}, nil
	}
	if err := host.Available(ctx); err != nil {
		if pluginContractBroken(err) {
			return nil, err
		}
		if requiresExternalReview(sctx) {
			return ciCheckReadFailureOutcome(err), nil
		}
		sctx.Log(fmt.Sprintf("skipping CI: %v", err))
		return &pipeline.StepOutcome{Skipped: true, SkipReason: err.Error()}, nil
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
		if requiresExternalReview(sctx) {
			return ciCheckReadFailureOutcome(fmt.Errorf("required external review has no PR URL")), nil
		}
		sctx.Log("no PR URL found, skipping CI")
		return &pipeline.StepOutcome{Skipped: true, SkipReason: "no PR URL found"}, nil
	}

	prNumber, err := scm.ExtractPRNumber(prURL)
	if err != nil {
		return nil, fmt.Errorf("extract PR number: %w", err)
	}
	pr := &scm.PR{Number: prNumber, URL: prURL}
	if retryRefusal {
		if err := clearCIMonitorReadyForIssues(sctx); err != nil {
			return nil, err
		}
		repair, err := s.retryProtectedPathRepair(sctx)
		if err != nil {
			return nil, err
		}
		retryRefusal = false
		if repair.Revalidate {
			return &pipeline.StepOutcome{RestartFrom: repair.restartFrom()}, nil
		}
	}
	baseBranch := effectivePRBaseBranch(sctx)
	// A resumed run may have a different trusted configuration than the run
	// that created this PR. Re-read the forge record without a base filter so
	// conflict repair and tip monitoring follow the PR's actual target.
	if reader, ok := host.(scm.PRBaseBranchReader); ok {
		if actual, readErr := reader.GetPRBaseBranch(ctx, pr); readErr == nil {
			pr.BaseBranch = actual
		} else if pluginContractBroken(readErr) {
			// A one-time read, not a poll: even a timeout fails closed,
			// since falling back to the configured base could monitor or
			// repair against a target the PR no longer has.
			return nil, readErr
		}
	}
	if strings.TrimSpace(pr.BaseBranch) != "" {
		baseBranch = strings.TrimSpace(pr.BaseBranch)
	}
	if repairRequested {
		repairOutcome, err := s.repairFromFindings(sctx, host, pr)
		if err != nil {
			return nil, err
		}
		if repairOutcome != nil {
			return repairOutcome, nil
		}
		// A repair that recorded nothing (the fixer failed) must not fall
		// through to monitoring the old published head while a carried review
		// approval still awaits its restart at Test: green checks there would
		// complete the run with the carried head never published. A commit the
		// failed fixer still left on top of the carried head goes through
		// recordRepair like any other, so it revokes the approval and restarts
		// at Review; only the unchanged carried head restarts at Test.
		pending, err := sctx.DB.RunTestRestartPending(sctx.Run.ID)
		if err != nil {
			return nil, err
		}
		if pending {
			head, err := stepGitHeadSHA(sctx)
			if err != nil {
				return nil, fmt.Errorf("resolve head after failed CI fix: %w", err)
			}
			repair, err := s.recordRepair(sctx, head)
			if err != nil {
				return nil, err
			}
			return &pipeline.StepOutcome{RestartFrom: repair.restartFrom(), Findings: sctx.DeferredFindings}, nil
		}
	}

	// CITimeout semantics: <0 (or "unlimited" in config) means never
	// self-terminate; 0 means the value was never configured, so fall back
	// to the default; >0 is an explicit finite idle timeout.
	timeout := sctx.Config.CITimeout
	unlimited := timeout < 0
	if timeout == 0 {
		timeout = config.DefaultCITimeout
	}

	releaseAfterChecksPass := ciReleaseAfterChecksPass(sctx)
	if releaseAfterChecksPass {
		sctx.Log(fmt.Sprintf("monitoring CI for PR #%s until every check is green...", prNumber))
	} else if unlimited {
		sctx.Log(fmt.Sprintf("monitoring CI for PR #%s (no timeout, until merged or closed)...", prNumber))
	} else {
		sctx.Log(fmt.Sprintf("monitoring CI for PR #%s (timeout: %s)...", prNumber, timeout))
	}
	// State the repair policy once, at entry, rather than at every poll: which
	// of the two very differently priced paths a repair will take is the single
	// most useful thing to know when reading a CI step log after the fact, and
	// it cannot be inferred from the repair line alone until a repair happens.
	sctx.Log(fmt.Sprintf("CI repair policy: %s (ci.revalidate_repairs: %t)", ciRepairPolicyDescription(sctx), ciRevalidatesRepairs(sctx)))
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
	// behindBaseTip is the base tip a green, BEHIND PR was last seen at. The
	// rebase repair fires only when the next poll still sees it green and
	// BEHIND at that same tip, so a base that keeps moving does not spend a
	// rebase and a full revalidation on a head that is stale again at once.
	behindBaseTip := ""
	mergeabilityBlockedReason := ""
	var timeoutFailingChecks []scm.CheckTarget
	lastCheckHeadSHA := sctx.Run.HeadSHA
	var timeoutExternalReviews []scm.Check
	timeoutMergeConflict := false
	lastMonitorLog := ciMonitorStatus{}
	consecutiveCheckErrs := 0
	consecutiveBotCommentErrs := 0
	timeoutOutcome := func() (*pipeline.StepOutcome, error) {
		if handoffPending(sctx) {
			if err := clearCIMonitorReady(sctx); err != nil {
				return nil, err
			}
		}
		sctx.Log("CI timeout reached")
		var outcome *pipeline.StepOutcome
		if len(timeoutFailingChecks) > 0 || timeoutMergeConflict {
			outcome = ciFailureOutcome(timeoutFailingChecks, timeoutMergeConflict, "CI timed out with known failures still present")
		} else if mergeabilityBlockedReason != "" {
			outcome = ciMergeabilityOutcome("mergeability check timed out", mergeabilityBlockedReason)
		} else {
			outcome = ciMonitoringTimeoutOutcome()
		}
		if len(timeoutExternalReviews) > 0 {
			external := ciObservationFindings(ciIssues{requiredReviews: requiredReviews(sctx), checks: timeoutExternalReviews, failing: failingCheckNames(timeoutExternalReviews)})
			if len(timeoutFailingChecks) > 0 || timeoutMergeConflict || mergeabilityBlockedReason != "" {
				prior, _ := types.ParseFindingsJSON(outcome.Findings)
				external.Items = append(prior.Items, external.Items...)
				external.Summary = prior.Summary + "; " + external.Summary
			}
			outcome = ciObservationOutcome(external)
		}
		return ciTerminalRepairOutcome(outcome, Findings{}, sctx.DeferredFindings), nil
	}
	logStatus := func(message string) error {
		var err error
		lastMonitorLog, err = logCIMonitorStatus(sctx, message, lastMonitorLog)
		return err
	}
	waitForPoll := func() (bool, error) {
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
		if s.waitForNextPoll != nil {
			return false, s.waitForNextPoll(ctx, interval)
		}
		timer := time.NewTimer(interval)
		defer timer.Stop()
		select {
		case <-timer.C:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()
		case <-sctx.CIWake:
			return true, nil
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pollBaseTip := ""
		botCommentReadFailed := false
		behindObserved := false

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
				pollBaseTip = tip
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
		if err != nil && pluginPollFailsStep(err) {
			if err := clearCIMonitorReady(sctx); err != nil {
				return nil, err
			}
			return nil, err
		}
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: could not check PR state: %v", err))
			prStateKnown = false
		} else if state == scm.PRStateMerged {
			if err := verifyMergedProof(ctx, host, pr, sctx.Run.HeadSHA); err != nil {
				return nil, err
			}
			if err := setCIMonitorPRState(sctx, "merged"); err != nil {
				return nil, err
			}
			notifyPRMerged(sctx)
			sctx.Log("PR has been merged!")
			return &pipeline.StepOutcome{}, nil
		} else if state == scm.PRStateClosed {
			if err := setCIMonitorPRState(sctx, "closed"); err != nil {
				return nil, err
			}
			sctx.Log("PR has been closed")
			return &pipeline.StepOutcome{}, nil
		} else if state == scm.PRStateOpen {
			if err := setCIMonitorPRState(sctx, "open"); err != nil {
				return nil, err
			}
		}

		// Check mergeable state if the provider supports it
		mergeConflict := false
		mergeBehind := false
		mergeabilityKnown := true
		if host.Capabilities().MergeableState {
			mergeState, mergeErr := host.GetMergeableState(ctx, pr)
			if mergeErr != nil && pluginPollFailsStep(mergeErr) {
				if err := clearCIMonitorReady(sctx); err != nil {
					return nil, err
				}
				return nil, mergeErr
			}
			if mergeErr != nil {
				sctx.Log(fmt.Sprintf("warning: could not check mergeable state: %v", mergeErr))
				mergeabilityBlockedReason = ""
				mergeabilityKnown = false
			} else {
				mergeConflict = mergeState.Conflict()
				// Unless ci.rebase_behind opts in, BEHIND reads as MERGEABLE:
				// the green PR reports checks-passed and waits for a human.
				mergeBehind = mergeState.Behind() && sctx.Config != nil && sctx.Config.CI.RebaseBehind
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

		// Check CI status - wait for all checks to complete before escalating
		pr.HeadSHA = sctx.Run.HeadSHA
		checks, observedHead, _, err := getCIChecks(ctx, host, pr)
		if observedHead != "" && observedHead != lastCheckHeadSHA {
			timeoutFailingChecks = nil
			lastCheckHeadSHA = observedHead
		}
		if err == nil {
			checks, err = requiredReviewChecks(sctx, host, pr, checks)
		}
		if err != nil && pluginPollFailsStep(err) {
			if err := clearCIMonitorReady(sctx); err != nil {
				return nil, err
			}
			return nil, err
		}
		if err != nil {
			if errors.Is(err, scm.ErrHeadChanged) {
				// The provider answered, but nothing it offered is evidence for
				// the commit this run is delivering: no pipeline has registered
				// for that commit yet, and the pipeline the merge request still
				// points at belongs to an older one. That state resolves when
				// the provider creates the pipeline, so it is a wait, not a
				// read failure - counting it toward the consecutive-error limit
				// would park the run after a few minutes telling the operator to
				// check a CLI that answered fine. The idle timeout still bounds
				// the wait, and the readiness signal is cleared because the
				// previous observation was for a different head.
				consecutiveCheckErrs = 0
				timeoutFailingChecks = nil
				if err := logStatus(fmt.Sprintf("%s%v", cimonitor.ChecksHeadWaitingPrefix, err)); err != nil {
					return nil, err
				}
			} else {
				if err := clearCIMonitorReady(sctx); err != nil {
					return nil, err
				}
				lastMonitorLog = ciMonitorStatus{}
				sctx.Log(fmt.Sprintf("warning: could not check CI: %v", err))
				consecutiveCheckErrs++
				// A provider read that keeps failing (e.g. gh < v2.50 rejecting
				// `gh pr checks --json`) must become an actionable stop, not an
				// invisible spin to ci_timeout. The PR state check above returned
				// already for merged/closed, so reaching here means the PR is open.
				if consecutiveCheckErrs >= consecutiveCheckErrorLimit {
					sctx.Log(fmt.Sprintf("CI checks could not be read %d consecutive times, parking for a decision", consecutiveCheckErrs))
					return ciTerminalRepairOutcome(ciCheckReadFailureOutcome(err), Findings{}, sctx.DeferredFindings), nil
				}
			}
		} else {
			consecutiveCheckErrs = 0
			timeoutExternalReviews = nil
			for _, check := range checks {
				if check.ExternalReview != "" && check.Bucket != scm.CheckBucketPass {
					check.Bucket = scm.CheckBucketFail
					check.State += " (CI TIMEOUT)"
					timeoutExternalReviews = append(timeoutExternalReviews, check)
				}
			}
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
			// rerun policy, even though it is not a verdict on the code. A
			// check held for maintainer approval will not finish on its own,
			// so it does not defer the other checks' issues.
			checksPending := hasExecutingPendingChecks(checks)
			// readinessPending is deliberately broader: any state that is not a
			// conclusive pass, failure, or skip must keep the PR non-ready. This
			// includes cancelled, held, and unknown provider states.
			readinessPending := hasPendingChecks(checks) || hasUnresolvedChecks(checks)
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
					if err := clearCIMonitorReady(sctx); err != nil {
						return nil, err
					}
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
			timeoutFailingChecks = terminalCheckTargetsForNames(checks, mergeCheckNames(reportedIssues, awaitingRerun))
			// Synthetic review requirements are decisions, including at timeout;
			// never hand their IDs to the generic job-repair outcome.
			for _, check := range checks {
				if check.ExternalReview != "" {
					for i := len(timeoutFailingChecks) - 1; i >= 0; i-- {
						if timeoutFailingChecks[i].ProviderID == check.ProviderID {
							timeoutFailingChecks = append(timeoutFailingChecks[:i], timeoutFailingChecks[i+1:]...)
						}
					}
				}
			}

			if hasIssues || len(awaitingRerun) > 0 {
				if err := clearCIMonitorReadyForIssues(sctx); err != nil {
					return nil, err
				}
			}
			if rerunIssued || (!hasIssues && len(awaitingRerun) > 0) {
				// The re-run checks are running again for the same commit, so
				// the monitor waits rather than escalating. This also clears any
				// previous passed-checks signal, which matters for a cancelled
				// check: it never counted as a failing check, so nothing above
				// cleared it.
				if err := logStatus(ciChecksRunningMsg); err != nil {
					return nil, err
				}
			} else if hasIssues && checksPending {
				// Issue handling waits only for checks that can still complete on
				// their own. A cancelled check whose rerun budget is exhausted must
				// reach the approval gate instead of waiting forever.
				lastMonitorLog = ciMonitorStatus{}
				if pendingCheckMatchesLastFixed(checks, s.lastFixedChecks) {
					s.lastFixedChecks = ""
					s.lastFixedCompletedAt = nil
				}
				sctx.Log("issues detected but checks still pending, waiting for all checks to complete...")
			} else if hasIssues {
				lastMonitorLog = ciMonitorStatus{}
				if mergeConflict && unlimited {
					tipCtx, cancel := context.WithTimeout(ctx, defaultBaseBranchTipResolveWindow)
					if tip, resolved := baseBranchTip(tipCtx); resolved {
						pollBaseTip = tip
					}
					cancel()
				}
				if s.lastRepairStillUnverified(checks, mergeConflict, pollBaseTip) {
					// The provider has not re-run the checks the last
					// published repair targeted: the failures on screen are
					// the ones that repair was for, not a verdict on it.
					// Escalating them again would spend a fix round on stale
					// evidence, so the monitor keeps waiting for the re-run.
					sctx.Log("fix already attempted for these issues, waiting for CI re-run...")
				} else {
					// Every check has finished and every authorized rerun is
					// spent: this poll is a settled observation. Each issue
					// becomes one finding carrying its action, and the
					// executor's shared findings machinery decides what
					// happens next - auto-fix findings enter the auto_fix.ci
					// loop as fix rounds, ask-user findings park for a
					// decision, and a poll with only ask-user findings (a
					// transient check no rerun will replace, a review bot's
					// comments) never starts a round.
					s.lastFixedChecks = ""
					s.lastFixedCompletedAt = nil
					sctx.DeferredFindings = ""
					s.observedCompletedAt = terminalFailureCompletionTimes(checks)
					findings := ciObservationFindings(ciIssues{
						provider:            host.Provider(),
						requiredReviews:     requiredReviews(sctx),
						checks:              checks,
						failing:             failing,
						unresolvedCancelled: unresolvedCancelled,
						mergeConflict:       mergeConflict,
						reruns:              s.transientReruns.used,
						botComments:         reviewBotComments(sctx, host, pr, checks),
						greenBots:           reviewBotCommentsAlways(sctx),
					})
					if hold := humanHoldMessage(checks); hold != "" {
						sctx.Log(hold)
					}
					sctx.Log(fmt.Sprintf("issues detected: %s", findings.Summary))
					return ciObservationOutcome(findings), nil
				}
			} else {
				s.lastFixedChecks = ""
				s.lastFixedCompletedAt = nil
				switch {
				case !prStateKnown || !mergeabilityKnown:
					if err := clearCIMonitorReady(sctx); err != nil {
						return nil, err
					}
					lastMonitorLog = ciMonitorStatus{}
				case readinessPending:
					// Checks are (re-)running with no failures yet. Surface this
					// so a PR that passed checks and starts re-running clears the
					// previous passed-checks signal instead of looking stale.
					// The broader readiness state is intentional here: cancelled
					// and unknown checks must never be promoted as green.
					// Applies even when no_ci is declared: registered checks are
					// never waived.
					if err := logStatus(ciWaitingMessage(checks)); err != nil {
						return nil, err
					}
				case len(checks) == 0 && (sctx.Config == nil || !sctx.Config.NoCI):
					// Empty forge results are ready ONLY with positive durable
					// evidence from trusted default-branch config (no_ci: true).
					// Without that declaration, keep waiting - delayed registration
					// is common and must never look green. Elapsed time is not
					// evidence; there is no grace-period promotion path.
					if err := clearCIMonitorReady(sctx); err != nil {
						return nil, err
					}
					lastMonitorLog = ciMonitorStatus{}
					sctx.Log("no CI checks reported yet, waiting for checks to register...")
				case len(checks) == 0 || allChecksPassed(checks):
					// Under ci.review_bot_comments: always, a review bot that
					// concluded its check green, or has not registered one on
					// this head yet, may still have left unresolved comments. They are read before the head is
					// reported ready, and become the same ask-user findings a
					// red bot check produces, so checks-passed is never reported
					// over a comment nobody has looked at. A trusted no_ci head
					// with no checks at all reads them the same way.
					botFindings, botErr := s.greenReviewBotFindings(sctx, host, pr, checks)
					if botErr != nil && pluginPollFailsStep(botErr) {
						if err := clearCIMonitorReady(sctx); err != nil {
							return nil, err
						}
						return nil, botErr
					}
					if botErr != nil {
						if err := clearCIMonitorReady(sctx); err != nil {
							return nil, err
						}
						lastMonitorLog = ciMonitorStatus{}
						botCommentReadFailed = true
						consecutiveBotCommentErrs++
						sctx.Log(fmt.Sprintf("warning: could not read review bot comments: %v", botErr))
						if consecutiveBotCommentErrs >= consecutiveCheckErrorLimit {
							sctx.Log(fmt.Sprintf("review bot comments could not be read %d consecutive times, parking for a decision", consecutiveBotCommentErrs))
							return ciTerminalRepairOutcome(reviewBotCommentsReadFailureOutcome(botErr), Findings{}, sctx.DeferredFindings), nil
						}
						break
					}
					if len(botFindings.Items) > 0 {
						if err := clearCIMonitorReady(sctx); err != nil {
							return nil, err
						}
						sctx.DeferredFindings = ""
						s.observedCompletedAt = terminalFailureCompletionTimes(checks)
						sctx.Log(fmt.Sprintf("issues detected: %s", botFindings.Summary))
						return ciObservationOutcome(botFindings), nil
					}
					if mergeBehind {
						// Green checks the forge still will not merge: the
						// base requires branches to be up to date. The
						// merge-conflict repair rebases onto the PR base.
						behindObserved = true
						tipCtx, cancel := context.WithTimeout(ctx, defaultBaseBranchTipResolveWindow)
						tip, resolved := baseBranchTip(tipCtx)
						cancel()
						if !resolved || tip == "" || tip != behindBaseTip {
							behindBaseTip = ""
							if resolved {
								behindBaseTip = tip
							}
							if err := logStatus(ciBehindBaseMsg); err != nil {
								return nil, err
							}
							break
						}
						if err := clearCIMonitorReady(sctx); err != nil {
							return nil, err
						}
						sctx.DeferredFindings = ""
						findings := ciObservationFindings(ciIssues{provider: host.Provider(), requiredReviews: requiredReviews(sctx), checks: checks, behindBase: true})
						sctx.Log(fmt.Sprintf("issues detected: %s", findings.Summary))
						return ciObservationOutcome(findings), nil
					}
					sctx.DeferredFindings = ""
					declaredNoCI := len(checks) == 0
					// The default keeps watching: the green observation only
					// clears the failure signal, as it always has, so the run
					// stays open until the PR is merged, closed, or declined.
					// ci.release_after_checks_pass opts into finishing here
					// instead, which records the checks verdict and releases the
					// run's worktree and lane.
					if releaseAfterChecksPass {
						completeMsg := ciChecksPassedCompleteMsg
						if declaredNoCI {
							completeMsg = ciNoChecksPassedCompleteMsg
						}
						sctx.Log(completeMsg)
						sctx.Log("validation complete: releasing the run now; the PR is ready for a human to merge")
						return &pipeline.StepOutcome{CIReadyNoCI: &declaredNoCI}, nil
					}
					passedMsg := ciChecksPassedMsg
					if declaredNoCI {
						passedMsg = ciNoChecksPassedMsg
					}
					if err := logStatus(passedMsg); err != nil {
						return nil, err
					}
				default:
					if err := clearCIMonitorReady(sctx); err != nil {
						return nil, err
					}
					if err := logStatus(ciChecksRunningMsg); err != nil {
						return nil, err
					}
				}
			}
		}

		if !botCommentReadFailed {
			consecutiveBotCommentErrs = 0
		}
		if !behindObserved {
			behindBaseTip = ""
		}
		woken, err := waitForPoll()
		if err != nil {
			return nil, err
		}
		if woken {
			lastMonitorLog = ciMonitorStatus{}
		}
	}
}

// humanHoldMessage names the hold a check set is under, or "" when every
// pending check can still finish on its own. The two holds read differently to
// the person they wait on - approve a workflow versus run a job - so
// each keeps its own wording instead of one hold's message describing the
// other's.
func humanHoldMessage(checks []scm.Check) string {
	if hasAwaitingApprovalChecks(checks) {
		return ciChecksAwaitingApprovalMsg
	}
	if hasAwaitingManualJobChecks(checks) {
		return ciChecksAwaitingManualJobMsg
	}
	return ""
}

// ciWaitingMessage names what the monitor is waiting for when no check has
// failed. A check the provider is holding for a person - a workflow awaiting
// maintainer approval, or a blocking manual job - has run nothing more and
// will run nothing until that person acts, so calling it "checks running"
// describes work that does not exist and hides the one thing that would move
// the run along. Anything else is an ordinary wait on checks in flight.
func ciWaitingMessage(checks []scm.Check) string {
	if hold := humanHoldMessage(checks); hold != "" {
		return hold
	}
	return ciChecksRunningMsg
}

// Keep logging independent of persistence so a failed write retries silently.
type ciMonitorStatus struct {
	persisted string
	logged    string
}

func logCIMonitorStatus(sctx *pipeline.StepContext, message string, previous ciMonitorStatus) (ciMonitorStatus, error) {
	if message != previous.persisted {
		pending := handoffPending(sctx)
		ready, declaredNoCI := ciReadyMessage(message)
		if err := setCIMonitorReadiness(sctx, ready, declaredNoCI); err != nil {
			if errors.Is(err, pipeline.ErrHandoff) {
				return previous, err
			}
			if message != previous.logged || pending {
				sctx.Log(fmt.Sprintf("warning: could not persist CI readiness: %v", err))
				sctx.Log(message)
			}
			previous.logged = message
			return previous, nil
		}
		if message != previous.logged || pending {
			sctx.Log(message)

		}
	}
	return ciMonitorStatus{persisted: message, logged: message}, nil
}

func ciReadyMessage(message string) (ready, declaredNoCI bool) {
	switch message {
	case ciChecksPassedMsg:
		return true, false
	case ciNoChecksPassedMsg:
		return true, true
	default:
		return false, false
	}
}

// ciReleaseAfterChecksPass reports whether the operator opted into finishing the
// run at the checks verdict. The default is off: the monitor keeps watching the
// PR until it is merged or closed, unless an operator explicitly hands it off.
func ciReleaseAfterChecksPass(sctx *pipeline.StepContext) bool {
	return sctx != nil && sctx.Config != nil && sctx.Config.CI.ReleaseAfterChecksPass
}

func handoffPending(sctx *pipeline.StepContext) bool {
	return sctx.HandoffPending != nil && sctx.HandoffPending()
}

// These two issue paths failed closed before handoff was introduced.
func clearCIMonitorReadyForIssues(sctx *pipeline.StepContext) error {
	if handoffPending(sctx) {
		return clearCIMonitorReady(sctx)
	}
	return setCIMonitorReadiness(sctx, false, false)
}

func clearCIMonitorReady(sctx *pipeline.StepContext) error {
	if err := setCIMonitorReadiness(sctx, false, false); err != nil {
		if errors.Is(err, pipeline.ErrHandoff) {
			return err
		}
		sctx.Log(fmt.Sprintf("warning: could not clear CI readiness: %v", err))
	}
	return nil

}

func setCIMonitorPRState(sctx *pipeline.StepContext, state string) error {
	if sctx.SetPRState != nil {
		return sctx.SetPRState(state)
	}
	return sctx.DB.UpdateRunPRState(sctx.Run.ID, state)
}

func setCIMonitorReadiness(sctx *pipeline.StepContext, ready, declaredNoCI bool) error {
	declaredNoCI = ready && declaredNoCI
	if sctx.SetCIReadiness != nil {
		return sctx.SetCIReadiness(ready, declaredNoCI)
	}
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
