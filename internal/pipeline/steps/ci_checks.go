package steps

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type lastFixedIssues struct {
	Checks        []scm.CheckTarget `json:"checks,omitempty"`
	MergeConflict bool              `json:"mergeConflict,omitempty"`
}

// pollInterval returns the polling interval based on elapsed time since CI monitoring started.
// 30s for first 5min, 60s for 5-15min, 120s after.
func pollInterval(elapsed time.Duration) time.Duration {
	switch {
	case elapsed < 5*time.Minute:
		return 30 * time.Second
	case elapsed < 15*time.Minute:
		return 60 * time.Second
	default:
		return 120 * time.Second
	}
}

// hasFailingChecks returns true if any CI check is in the fail bucket.
func hasFailingChecks(checks []scm.Check) bool {
	for _, c := range checks {
		if c.Failing() {
			return true
		}
	}
	return false
}

// hasPendingChecks returns true if any CI check is still running or queued.
func hasPendingChecks(checks []scm.Check) bool {
	for _, c := range checks {
		if c.Pending() {
			return true
		}
	}
	return false
}

func hasUnresolvedChecks(checks []scm.Check) bool {
	for _, c := range checks {
		switch c.Bucket {
		case scm.CheckBucketPass, scm.CheckBucketFail, scm.CheckBucketSkip:
		default:
			return true
		}
	}
	return false
}

func allChecksPassed(checks []scm.Check) bool {
	if len(checks) == 0 {
		return false
	}
	for _, c := range checks {
		if c.Bucket != scm.CheckBucketPass && c.Bucket != scm.CheckBucketSkip {
			return false
		}
	}
	return true
}

// failingCheckNames returns the names of failing checks.
func failingCheckNames(checks []scm.Check) []string {
	var names []string
	for _, c := range checks {
		if c.Failing() {
			names = append(names, c.Name)
		}
	}
	return names
}

// unresolvedCheckNames returns "name (bucket)" for every check that does not
// count toward allChecksPassed - i.e. everything but CheckBucketPass and
// CheckBucketSkip. Unlike failingCheckNames, this also surfaces pending,
// cancelled, and any other non-terminal-pass bucket, so a caller reporting an
// approval override can name pending/unknown checks, not just failing ones.
func unresolvedCheckNames(checks []scm.Check) []string {
	var names []string
	for _, c := range checks {
		if c.Bucket == scm.CheckBucketPass || c.Bucket == scm.CheckBucketSkip {
			continue
		}
		names = append(names, fmt.Sprintf("%s (%s)", c.Name, c.Bucket))
	}
	return names
}

// terminalFailureCompletionTimes snapshots when each terminally failed check
// finished, so a later poll can tell that CI has re-run since the fix push.
//
// It covers the whole terminal-failure set rather than just the fail bucket
// because a cancelled check can be a fix target too (see the CI step's
// fixTargets). Keying the snapshot on the fail bucket alone would leave a
// cancelled-only fix round with no completion evidence at all, and the step
// would then have no way to notice its own re-run.
func terminalFailureCompletionTimes(checks []scm.Check) map[string]time.Time {
	completedAt := make(map[string]time.Time)
	for _, c := range checks {
		if !checkFailedTerminally(c) {
			continue
		}
		if c.CompletedAt.IsZero() {
			continue
		}
		key := checkTrackingKey(c)
		previous := completedAt[key]
		if previous.IsZero() || c.CompletedAt.After(previous) {
			completedAt[key] = c.CompletedAt
		}
	}
	if len(completedAt) == 0 {
		return nil
	}
	return completedAt
}

func completionTimesForTargets(completedAt map[string]time.Time, targets []scm.CheckTarget) map[string]time.Time {
	selected := make(map[string]time.Time, len(targets))
	for _, target := range targets {
		key := checkTargetTrackingKey(target)
		if completed := completedAt[key]; !completed.IsZero() {
			selected[key] = completed
		}
	}
	if len(selected) == 0 {
		return nil
	}
	return selected
}

func terminalFailureCompletedAfter(checks []scm.Check, after map[string]time.Time) bool {
	if len(after) == 0 {
		return false
	}
	for _, c := range checks {
		if !checkFailedTerminally(c) || c.CompletedAt.IsZero() {
			continue
		}
		previous, ok := after[checkTrackingKey(c)]
		if ok && c.CompletedAt.After(previous) {
			return true
		}
	}
	return false
}

func pendingCheckMatchesLastFixed(checks []scm.Check, lastFixedChecks string) bool {
	issues, ok := decodeLastFixedChecks(lastFixedChecks)
	if !ok {
		return false
	}

	if len(issues.Checks) == 0 {
		return issues.MergeConflict && hasPendingChecks(checks)
	}

	for _, c := range checks {
		if !c.Pending() {
			continue
		}
		for _, target := range issues.Checks {
			if checkMatchesTarget(c, target) {
				return true
			}
		}
	}

	return false
}

func encodeLastFixedChecks(checks []scm.CheckTarget, mergeConflict bool) string {
	if len(checks) == 0 && !mergeConflict {
		return ""
	}
	encoded, err := json.Marshal(lastFixedIssues{Checks: checks, MergeConflict: mergeConflict})
	if err != nil {
		return ""
	}
	return string(encoded)
}

func decodeLastFixedChecks(raw string) (lastFixedIssues, bool) {
	if raw == "" {
		return lastFixedIssues{}, false
	}
	var issues lastFixedIssues
	if err := json.Unmarshal([]byte(raw), &issues); err != nil {
		return lastFixedIssues{}, false
	}
	if len(issues.Checks) == 0 && !issues.MergeConflict {
		return lastFixedIssues{}, false
	}
	return issues, true
}

// lastRepairStillUnverified reports whether every issue the last published
// repair targeted is still terminally failed, meaning the provider has not
// yet re-run those checks against the repaired head. The two clears that
// prove a re-run happened - a newer completion (terminalFailureCompletedAfter)
// and the fixed check observed pending (pendingCheckMatchesLastFixed) - empty
// lastFixedChecks before this is asked, so an unverified repair is exactly
// one whose targets are all still red as they were. A target that cleared, or
// a conflict that resolved, is the provider acting on the repair and makes
// the observation fresh.
func (s *CIStep) lastRepairStillUnverified(checks []scm.Check, mergeConflict bool) bool {
	issues, ok := decodeLastFixedChecks(s.lastFixedChecks)
	if !ok {
		return false
	}
	if issues.MergeConflict && !mergeConflict {
		return false
	}
	for _, target := range issues.Checks {
		if !checkTargetFailedTerminally(checks, target) {
			return false
		}
	}
	return true
}

func checkTargetFailedTerminally(checks []scm.Check, target scm.CheckTarget) bool {
	for _, check := range checks {
		if checkMatchesTarget(check, target) && checkFailedTerminally(check) {
			return true
		}
	}
	return false
}

func checkMatchesTarget(check scm.Check, target scm.CheckTarget) bool {
	if target.ProviderID != "" {
		return check.ProviderID == target.ProviderID
	}
	return check.Name == target.Name
}

func checkTrackingKey(check scm.Check) string {
	return checkTargetTrackingKey(scm.CheckTarget{Name: check.Name, ProviderID: check.ProviderID})
}

func checkTargetTrackingKey(target scm.CheckTarget) string {
	if target.ProviderID != "" {
		return "id:" + target.ProviderID
	}
	return target.Name
}

// ciFailureOutcome parks the step over issues that are still present when
// the idle timeout ends monitoring. Nothing here is a fresh observation the
// executor could act on, so every item is ask-user.
func ciFailureOutcome(failing []string, mergeConflict bool, summary string) *pipeline.StepOutcome {
	findings := Findings{Summary: summary}
	for _, name := range failing {
		findings.Items = append(findings.Items, Finding{
			Severity:    "warning",
			Description: fmt.Sprintf("CI check failing: %s", name),
			Action:      types.ActionAskUser,
			Category:    types.FindingCategoryCICheck,
			Check:       name,
		})
	}
	if mergeConflict {
		findings.Items = append(findings.Items, Finding{
			Severity:    "warning",
			Description: "PR has merge conflicts with the base branch",
			Action:      types.ActionAskUser,
			Category:    types.FindingCategoryCIMergeConflict,
		})
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

// consecutiveCheckErrorLimit bounds consecutive GetChecks failures before the
// CI step parks at an ask-user gate. At the default 30s poll this is ~3 minutes
// of a provider read that keeps failing, making a broken gh (e.g. < v2.50, which
// rejects `gh pr checks --json`) an actionable stop instead of an invisible
// spin to ci_timeout.
const consecutiveCheckErrorLimit = 6

// ConsecutiveCheckErrorLimit is the parked-after-N-failures bound. Tests in
// other packages share this so they cannot drift from the monitor's gate.
func ConsecutiveCheckErrorLimit() int { return consecutiveCheckErrorLimit }

func ciCheckReadFailureOutcome(err error) *pipeline.StepOutcome {
	findings := Findings{
		Summary: "CI checks could not be read from the provider",
		Items: []Finding{{
			Severity:    "warning",
			Description: fmt.Sprintf("CI checks could not be read from the provider: %v. Verify that the provider CLI or credentials are installed, authenticated, and support the required check-reading command. For GitHub errors involving 'pr checks --json', gh >= 2.50 is required.", err),
			Action:      types.ActionAskUser,
		}},
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

// ciFixAgentTimeoutOutcome parks the CI step for a decision after the auto-fix
// agent burned its whole invocation budget without finishing.
//
// The previous behaviour downgraded this to a log warning and let the poll loop
// issue the same request again on the next tick, up to auto_fix.ci attempts -
// each one another full agent budget, all of it invisible except for warning
// lines inside the CI step log, until ci_timeout ended the run hours later with
// nothing to act on. That is the same invisible spin consecutiveCheckErrorLimit
// exists to prevent, and repeating an invocation that has already proven it
// cannot finish is a blind retry, not a recovery.
//
// Parking instead is bounded (one budget, then a decision), keeps the run and
// its worktree alive rather than tearing them down, and leaves any further
// attempt to the operator, who can respond with a fix selection to spend
// another budget deliberately.
func ciFixAgentTimeoutOutcome(issueDesc string, dirtyWorktree string, err error) *pipeline.StepOutcome {
	description := fmt.Sprintf(
		"The CI auto-fix agent did not finish within its invocation budget while repairing: %s. "+
			"Reported: %v. Re-running the same request costs another full budget, so no further attempt is made automatically. "+
			"Check that the configured agent CLI is authenticated and responsive, then respond with a fix selection to spend another budget, or resolve the CI failure outside the pipeline.",
		issueDesc, err)
	if dirtyWorktree != "" {
		description += fmt.Sprintf(" The timed-out agent left uncommitted changes in the run worktree at %s; they are not committed or pushed.", dirtyWorktree)
	}
	findings := Findings{
		Summary: "CI auto-fix agent exceeded its invocation budget",
		Items: []Finding{{
			ID:          "ci-fix-agent-timeout",
			Severity:    "warning",
			Description: description,
			Action:      types.ActionAskUser,
		}},
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

func ciMergeabilityOutcome(summary, description string) *pipeline.StepOutcome {
	findings := Findings{
		Summary: summary,
		Items: []Finding{{
			Severity:    "warning",
			Description: description,
			Action:      types.ActionAskUser,
		}},
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

func ciMonitoringTimeoutOutcome() *pipeline.StepOutcome {
	findings := Findings{
		Summary: "CI monitoring timed out before PR was merged or closed",
		Items: []Finding{{
			Severity:    "warning",
			Description: "PR was still open when CI monitoring timed out",
			Action:      types.ActionAskUser,
		}},
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}
