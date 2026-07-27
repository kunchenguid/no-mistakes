package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// failureClass is how a terminally failed check should be treated before the
// CI step escalates it to the fix agent.
type failureClass string

const (
	// classGenuine is a failure the provider attributes to the job itself:
	// its own exit status, its own configured timeout, or a workflow that
	// could not start. Running it again on the same commit reproduces it, so
	// it goes straight to the fix agent.
	classGenuine failureClass = "genuine"
	// classTransient is an outcome the provider attributes to itself rather
	// than to the job. Only a cancelled check qualifies: nothing about the
	// commit produced it, so one rerun is worth more than an agent round.
	classTransient failureClass = "transient"
	// classUnknown is a check whose state the provider did not report, or one
	// this version does not recognize. It never earns a rerun: an
	// indeterminate state is not evidence of a transient one.
	classUnknown failureClass = "unknown"
)

// classifyCheckFailure maps a check to the class that decides whether a
// deterministic rerun is worth trying instead of an agent round. It reads the
// provider's own reported state - never check names or log text - so the
// decision is as trustworthy as the status API behind it. Checks that are not
// terminally failed classify as classUnknown: this function answers "how did
// this check fail", so a caller must only ask it about a failed check.
//
// TIMED_OUT is deliberately genuine. GitHub reports it when a job exceeds its
// own timeout-minutes, which is usually the branch's own code hanging, so a
// rerun burns another full timeout window reproducing it - and on a repo with a
// short ci_timeout it can turn an auto-fixable failure into a timeout gate.
// STALE is deliberately absent: normalizeCheckBucket in internal/scm/github
// treats a stale check as skipped rather than failed, and one mapping saying
// "not a failure" while another says "re-run it" would make the outcome depend
// on whether the provider happened to report a bucket.
func classifyCheckFailure(check scm.Check) failureClass {
	if !checkFailedTerminally(check) {
		return classUnknown
	}
	switch strings.ToUpper(strings.TrimSpace(check.State)) {
	case "CANCELLED", "CANCELED":
		return classTransient
	case "FAILURE", "FAILED", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return classGenuine
	default:
		// Includes the empty state: a provider that reported a failed bucket
		// without a state cannot tell us why, and "we do not know" is not
		// "transient".
		return classUnknown
	}
}

// checkFailedTerminally reports whether a check has finished in a state that
// keeps the PR from being green. The cancel bucket is included because a
// cancelled check is exactly the outcome a rerun exists for, even though it
// does not count as a failing check for escalation.
func checkFailedTerminally(check scm.Check) bool {
	return check.Failing() || check.Bucket == scm.CheckBucketCancel
}

// rerunRollupGracePolls is how many polls a check gets to show its rerun after
// one was requested for it. `gh run rerun` returns as soon as the provider
// accepts the request, while the new attempt replaces the cancelled check in
// the status rollup asynchronously, so the very next poll can still be reading
// the outcome the rerun was meant to replace. Without a grace the run would
// escalate a check it never actually re-ran; without a bound on that grace, a
// request the provider accepted but never reflected would keep the monitor
// waiting until its idle timeout.
const rerunRollupGracePolls = 2

// rerunRollupState is what a check looked like when its rerun was requested, so
// a later poll can tell the re-run job ending cancelled again from the rollup
// simply not having refreshed yet.
type rerunRollupState struct {
	completedAt    time.Time
	graceRemaining int
}

// checkRerunBudget records how many reruns each check has consumed during this
// run. It is keyed by check name so one flaky job cannot spend another job's
// budget, and it is spent on the attempt rather than on success, so a provider
// that keeps refusing the request cannot be retried in a loop.
//
// A check name is not guaranteed unique on a PR, so same-named checks share one
// key and therefore one budget. Selection must reserve against that shared key
// (see transientRerunCandidates) or a single poll could spend it more than once.
type checkRerunBudget struct {
	spent  map[string]int
	rollup map[string]rerunRollupState
}

func (b *checkRerunBudget) remaining(name string, limit int) int {
	if limit <= 0 {
		return 0
	}
	return limit - b.spent[name]
}

func (b *checkRerunBudget) used(name string) int { return b.spent[name] }

// spend records one rerun against check's name, along with the completion the
// check reported at that moment. That timestamp is the only evidence a later
// poll has that it is reading a refreshed rollup rather than the same outcome
// the rerun was requested for.
func (b *checkRerunBudget) spend(check scm.Check) int {
	if b.spent == nil {
		b.spent = map[string]int{}
	}
	if b.rollup == nil {
		b.rollup = map[string]rerunRollupState{}
	}
	b.spent[check.Name]++
	b.rollup[check.Name] = rerunRollupState{completedAt: check.CompletedAt, graceRemaining: rerunRollupGracePolls}
	return b.spent[check.Name]
}

// cancelledAfterRerun partitions the checks this run already spent a rerun on
// into the ones the provider cancelled again (unresolved: nothing but a
// decision can clear them) and the ones whose rerun it has not published yet
// (awaiting: the monitor keeps waiting rather than escalating an outcome the
// rerun was meant to replace).
//
// It consumes rollup grace as it goes, so it must be called at most once per
// poll. A check whose grace runs out is reported as unresolved: a provider that
// accepted a rerun and never reflected it must not stall the run.
func (b *checkRerunBudget) cancelledAfterRerun(checks []scm.Check) (unresolved, awaiting []string) {
	refreshed := map[string]bool{}
	var order []string
	for _, check := range checks {
		if check.Bucket != scm.CheckBucketCancel || b.used(check.Name) == 0 {
			continue
		}
		if _, seen := refreshed[check.Name]; !seen {
			order = append(order, check.Name)
		}
		// Same-named checks share one budget key, so one refreshed instance is
		// enough to say the name's rollup has moved on.
		refreshed[check.Name] = refreshed[check.Name] || !b.rollupUnchanged(check)
	}
	for _, name := range order {
		if refreshed[name] || !b.consumeRollupGrace(name) {
			unresolved = append(unresolved, name)
			continue
		}
		awaiting = append(awaiting, name)
	}
	return unresolved, awaiting
}

// rollupUnchanged reports whether check still carries the exact completion it
// reported when its rerun was requested. An unknown completion on either side
// is not evidence of anything, so it reads as changed and the check is treated
// exactly as it was before rollup lag was accounted for.
func (b *checkRerunBudget) rollupUnchanged(check scm.Check) bool {
	state, ok := b.rollup[check.Name]
	if !ok || state.completedAt.IsZero() || check.CompletedAt.IsZero() {
		return false
	}
	return check.CompletedAt.Equal(state.completedAt)
}

// consumeRollupGrace spends one poll of a check's rollup grace and reports
// whether any was left to spend.
func (b *checkRerunBudget) consumeRollupGrace(name string) bool {
	state, ok := b.rollup[name]
	if !ok || state.graceRemaining <= 0 {
		return false
	}
	state.graceRemaining--
	b.rollup[name] = state
	return true
}

// transientRerunCandidates returns the checks that should be re-run before any
// failure on this PR reaches the fix agent.
//
// It returns nothing when ANY terminally failed check is genuine or
// indeterminate. That check needs the fix agent, and it must get it on its
// first failure with no added latency, so a transient sibling never delays it.
// It also returns nothing once every transient check has spent its budget,
// which is what makes the second failure of the same check escalate.
func transientRerunCandidates(checks []scm.Check, budget *checkRerunBudget, limit int) []scm.Check {
	if limit <= 0 {
		return nil
	}
	var candidates []scm.Check
	// reserved counts what this selection has already promised to spend.
	// Two terminally failed checks can carry the same name - the same job name
	// in two workflow files, or a matrix leg the provider reports without a
	// distinguishing suffix - and they share one budget key, so reading only
	// what was spent on earlier polls would admit both and blow the limit on a
	// single poll.
	reserved := map[string]int{}
	for _, check := range checks {
		if !checkFailedTerminally(check) {
			continue
		}
		if classifyCheckFailure(check) != classTransient {
			return nil
		}
		if budget.remaining(check.Name, limit)-reserved[check.Name] > 0 {
			reserved[check.Name]++
			candidates = append(candidates, check)
		}
	}
	return candidates
}

// mergeCheckNames appends the names in extra that base does not already carry.
// It is how a cancelled check the provider never resolved joins the issues the
// step reports: a cancelled check is not a failing check, so on its own it
// never reaches a gate and the monitor would report a PR carrying it as green.
func mergeCheckNames(base, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]struct{}, len(base)+len(extra))
	for _, name := range base {
		seen[name] = struct{}{}
	}
	merged := base
	for _, name := range extra {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		merged = append(merged, name)
	}
	return merged
}

// rerunTransientChecks re-runs the checks the provider itself reported as
// cancelled, before their failure escalates to the fix agent. It returns true
// when at least one rerun was requested, and a terminal outcome when the
// published branch head no longer matches the commit this run delivered.
//
// Callers must not reach here while the PR has a merge conflict: no rerun can
// clear one, so it has to escalate on its first observation.
//
// Every failure path here falls back to the behavior this policy replaces: no
// rerun, and the failure escalates exactly as it would without it.
func (s *CIStep) rerunTransientChecks(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, checks []scm.Check) (bool, *pipeline.StepOutcome) {
	limit := sctx.Config.CI.RerunTransient
	if limit <= 0 {
		return false, nil
	}
	// The rerun capability is optional: providers without one keep behaving
	// exactly as they did before this policy existed.
	rerunner, ok := host.(scm.CheckRerunner)
	if !ok {
		return false, nil
	}
	candidates := transientRerunCandidates(checks, &s.transientReruns, limit)
	if len(candidates) == 0 {
		return false, nil
	}

	// A rerun runs the checks again for whatever commit the branch now points
	// at, so it is only meaningful while that is still the commit this run
	// delivered. If the branch moved, the checks being re-run would certify a
	// revision this run never produced: terminate with the evidence instead.
	published, err := publishedBranchHead(sctx)
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: could not verify the published branch head before re-running checks: %v", err))
		return false, nil
	}
	if published != sctx.Run.HeadSHA {
		sctx.Log(fmt.Sprintf("published branch head moved (expected %s, observed %s); not re-running checks", shortSHA(sctx.Run.HeadSHA), shortSHA(published)))
		return false, ciRerunHeadMismatchOutcome(sctx.Run.HeadSHA, published)
	}

	issued := false
	for _, check := range candidates {
		used := s.transientReruns.spend(check)
		if err := rerunner.RerunCheck(sctx.Ctx, pr, check); err != nil {
			sctx.Log(fmt.Sprintf("warning: could not re-run transient CI check %s: %v", check.Name, err))
			continue
		}
		issued = true
		// used can never exceed limit: selection reserved this rerun against
		// the same shared budget key before it was spent.
		sctx.Log(fmt.Sprintf("re-running CI check %s (%d/%d): provider reported %s, not a job failure", check.Name, used, limit, transientStateLabel(check)))
	}
	return issued, nil
}

// publishedBranchHead returns the commit the run's branch points at on the push
// target - the commit the provider's checks actually ran on. A branch that is
// missing there is an error like any other unreadable remote, never a SHA the
// caller could mistake for a real head.
//
// The ls-remote is bounded on its own deadline rather than the run's context,
// exactly like the base-branch tip resolution in the same poll loop: this runs
// once per poll, its failure path just declines the rerun, and the CI timeout is
// only evaluated at the top of the loop, so a git transport that hangs while the
// status API stays healthy would otherwise defer timeout detection indefinitely.
func publishedBranchHead(sctx *pipeline.StepContext) (string, error) {
	ref := normalizedBranchRef(sctx.Run.Branch)
	ctx, cancel := context.WithTimeout(sctx.Ctx, defaultPublishedHeadResolveWindow)
	defer cancel()
	bounded := *sctx
	bounded.Ctx = ctx
	out, err := stepGitRun(&bounded, "ls-remote", resolvePushURL(sctx), ref)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("%s is not present on the push target", ref)
	}
	return fields[0], nil
}

func transientStateLabel(check scm.Check) string {
	if state := strings.ToLower(strings.TrimSpace(check.State)); state != "" {
		return state
	}
	return string(check.Bucket)
}

// ciUnresolvedCancelledOutcome parks the run for checks the provider cancelled
// again after this run already spent their rerun budget.
//
// A cancellation is never a verdict on the code, so there is nothing for the fix
// agent to repair: routing it into the auto_fix.ci loop would spend an agent
// round - and let that agent edit code the provider never tested - chasing an
// outcome only the provider can clear. The findings are ask-user for the same
// reason, so a fix loop cannot pick them up later either.
func ciUnresolvedCancelledOutcome(names []string) *pipeline.StepOutcome {
	findings := Findings{Summary: "CI checks were cancelled again after their rerun"}
	for _, name := range names {
		findings.Items = append(findings.Items, Finding{
			Severity:    "warning",
			Description: fmt.Sprintf("CI check cancelled again after its rerun: %s - the provider cancelled it rather than reporting a job failure, so it needs a decision rather than a code fix", name),
			Action:      types.ActionAskUser,
		})
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

func ciRerunHeadMismatchOutcome(expected, observed string) *pipeline.StepOutcome {
	findings := Findings{
		Summary: "published branch head no longer matches the commit this run delivered",
		Items: []Finding{{
			Severity:    "warning",
			Description: fmt.Sprintf("CI checks could not be re-run: expected head %s, observed %s on the push target", expected, observed),
			Action:      types.ActionAskUser,
		}},
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}
