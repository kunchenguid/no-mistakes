package steps

import (
	"encoding/json"
	"fmt"
	"strings"

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

// checkRerunBudget records how many reruns each check has consumed during this
// run. It is keyed by check name so one flaky job cannot spend another job's
// budget, and it is spent on the attempt rather than on success, so a provider
// that keeps refusing the request cannot be retried in a loop.
//
// A check name is not guaranteed unique on a PR, so same-named checks share one
// key and therefore one budget. Selection must reserve against that shared key
// (see transientRerunCandidates) or a single poll could spend it more than once.
type checkRerunBudget struct {
	spent map[string]int
}

func (b *checkRerunBudget) remaining(name string, limit int) int {
	if limit <= 0 {
		return 0
	}
	return limit - b.spent[name]
}

func (b *checkRerunBudget) used(name string) int { return b.spent[name] }

func (b *checkRerunBudget) spend(name string) int {
	if b.spent == nil {
		b.spent = map[string]int{}
	}
	b.spent[name]++
	return b.spent[name]
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

// mergeUnresolvedCancelledChecks returns failing plus the names of checks that
// are still terminally cancelled after this run already spent a rerun on them.
//
// A cancelled check is not a failing check, so on its own it never reaches the
// gate a failing check does, and the monitor would report a PR carrying it as
// green. Once a rerun has been spent and the check came back cancelled anyway,
// it is an unresolved issue and belongs with the failing checks. Checks this run
// never re-ran - reruns disabled, or a provider with no rerun capability - are
// left exactly as they were before this policy existed.
func mergeUnresolvedCancelledChecks(failing []string, checks []scm.Check, budget *checkRerunBudget) []string {
	seen := make(map[string]struct{}, len(failing))
	for _, name := range failing {
		seen[name] = struct{}{}
	}
	merged := failing
	for _, check := range checks {
		if check.Bucket != scm.CheckBucketCancel || budget.used(check.Name) == 0 {
			continue
		}
		if _, ok := seen[check.Name]; ok {
			continue
		}
		seen[check.Name] = struct{}{}
		merged = append(merged, check.Name)
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
		used := s.transientReruns.spend(check.Name)
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
func publishedBranchHead(sctx *pipeline.StepContext) (string, error) {
	ref := normalizedBranchRef(sctx.Run.Branch)
	out, err := stepGitRun(sctx, "ls-remote", resolvePushURL(sctx), ref)
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
