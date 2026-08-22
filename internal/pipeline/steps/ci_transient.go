package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
	// A failure the provider produced before the repository's own steps ran is an
	// infrastructure outcome, not a verdict on the code: the job's setup phase
	// failed (e.g. a GitHub Actions action-download outage) so no repository step
	// executed. It is as re-runnable as a cancellation, and it can never mask a
	// real failure - a genuine test or lint failure cleared setup and failed a
	// later step, so it is never marked PreRunFailure.
	if check.PreRunFailure {
		return classTransient
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
	headSHA        string
	observedLinks  map[string]bool
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

// persistedRerunBudget is the on-disk shape of a checkRerunBudget. It is a
// named type rather than an inline literal so a field added here is a
// compile-time decision about what must survive a restart.
type persistedRerunBudget struct {
	Spent  map[string]int                  `json:"spent,omitempty"`
	Rollup map[string]persistedRollupState `json:"rollup,omitempty"`
}

type persistedRollupState struct {
	CompletedAt    time.Time `json:"completed_at"`
	GraceRemaining int       `json:"grace_remaining"`
	HeadSHA        string    `json:"head_sha,omitempty"`
	ObservedLinks  []string  `json:"observed_links,omitempty"`
}

// marshal renders the budget for persistence. An empty budget marshals to the
// empty string so a run that never spent a rerun writes nothing.
func (b *checkRerunBudget) marshal() (string, error) {
	if len(b.spent) == 0 && len(b.rollup) == 0 {
		return "", nil
	}
	payload := persistedRerunBudget{Spent: b.spent}
	if len(b.rollup) > 0 {
		payload.Rollup = make(map[string]persistedRollupState, len(b.rollup))
		for name, state := range b.rollup {
			observedLinks := make([]string, 0, len(state.observedLinks))
			for link := range state.observedLinks {
				observedLinks = append(observedLinks, link)
			}
			sort.Strings(observedLinks)
			payload.Rollup[name] = persistedRollupState{
				CompletedAt:    state.completedAt,
				GraceRemaining: state.graceRemaining,
				HeadSHA:        state.headSHA,
				ObservedLinks:  observedLinks,
			}
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal rerun budget: %w", err)
	}
	return string(encoded), nil
}

// unmarshal restores a budget persisted by marshal. An empty payload leaves the
// budget untouched, so a run that never spent a rerun starts clean.
func (b *checkRerunBudget) unmarshal(encoded string) error {
	if strings.TrimSpace(encoded) == "" {
		return nil
	}
	var payload persistedRerunBudget
	if err := json.Unmarshal([]byte(encoded), &payload); err != nil {
		return fmt.Errorf("unmarshal rerun budget: %w", err)
	}
	b.spent = payload.Spent
	if b.spent == nil {
		b.spent = map[string]int{}
	}
	b.rollup = make(map[string]rerunRollupState, len(payload.Rollup))
	for name, state := range payload.Rollup {
		observedLinks := make(map[string]bool, len(state.ObservedLinks))
		for _, link := range state.ObservedLinks {
			if link != "" {
				observedLinks[link] = true
			}
		}
		b.rollup[name] = rerunRollupState{
			completedAt:    state.CompletedAt,
			graceRemaining: state.GraceRemaining,
			headSHA:        state.HeadSHA,
			observedLinks:  observedLinks,
		}
	}
	return nil
}

func (b *checkRerunBudget) remaining(name string, limit int) int {
	if limit <= 0 {
		return 0
	}
	return limit - b.spent[name]
}

func (b *checkRerunBudget) used(name string) int { return b.spent[name] }

// spend records one rerun against check's name and the pipeline head it
// certifies, along with the completion reported at that moment.
func (b *checkRerunBudget) spend(check scm.Check, checks []scm.Check, headSHA string) int {
	if b.spent == nil {
		b.spent = map[string]int{}
	}
	if b.rollup == nil {
		b.rollup = map[string]rerunRollupState{}
	}
	b.spent[check.Name]++
	observedLinks := map[string]bool{}
	for _, observed := range checks {
		if observed.Name == check.Name && observed.Link != "" {
			observedLinks[observed.Link] = true
		}
	}
	b.rollup[check.Name] = rerunRollupState{
		completedAt:    check.CompletedAt,
		graceRemaining: rerunRollupGracePolls,
		headSHA:        headSHA,
		observedLinks:  observedLinks,
	}
	return b.spent[check.Name]
}

func (b *checkRerunBudget) retireResolvedReruns(checks []scm.Check, currentHead string, persist func(*checkRerunBudget) error) (bool, error) {
	if len(b.rollup) == 0 {
		return false, nil
	}
	retirable := map[string]bool{}
	for name, state := range b.rollup {
		if state.headSHA != "" && state.headSHA != currentHead {
			retirable[name] = true
		}
	}
	resolved := map[string]bool{}
	outstanding := map[string]bool{}
	for _, check := range checks {
		state, tracked := b.rollup[check.Name]
		if !tracked || retirable[check.Name] {
			continue
		}
		if checkFailedTerminally(check) && classifyCheckFailure(check) == classTransient {
			outstanding[check.Name] = true
			continue
		}
		switch check.Bucket {
		case scm.CheckBucketPass, scm.CheckBucketFail, scm.CheckBucketSkip:
			if check.Link != "" && !state.observedLinks[check.Link] {
				resolved[check.Name] = true
			}
		default:
			outstanding[check.Name] = true
		}
	}
	for name := range resolved {
		if !outstanding[name] {
			retirable[name] = true
		}
	}
	// A delayed same-named sibling can look identical to a rerun replacement
	// here. That name-keyed masking already exists on the default branch in
	// cancelledAfterRerun; distinguishing it requires provider truth outside
	// this policy.
	if len(retirable) == 0 {
		return false, nil
	}
	candidate := &checkRerunBudget{
		spent:  b.spent,
		rollup: make(map[string]rerunRollupState, len(b.rollup)-len(retirable)),
	}
	for name, state := range b.rollup {
		if !retirable[name] {
			candidate.rollup[name] = state
		}
	}
	if err := persist(candidate); err != nil {
		return false, err
	}
	for name := range retirable {
		delete(b.rollup, name)
	}
	return true, nil
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
	present := map[string]bool{}
	var order []string
	for _, check := range checks {
		if b.used(check.Name) == 0 {
			continue
		}
		// A rerun this run issued is outstanding until its replacement is
		// published, whatever bucket the check currently reports. Recording
		// presence for every bucket keeps a check that moved from cancelled to
		// running or success from being read as missing below.
		present[check.Name] = true
		if check.Bucket != scm.CheckBucketCancel {
			continue
		}
		if _, seen := refreshed[check.Name]; !seen {
			order = append(order, check.Name)
		}
		// Same-named checks share one budget key, so one refreshed instance is
		// enough to say the name's rollup has moved on.
		refreshed[check.Name] = refreshed[check.Name] || !b.rollupUnchanged(check)
	}

	// Outstanding reruns drive this, not the checks the latest response
	// happens to carry. A transitional rollup can omit the very check a rerun
	// was requested for; iterating only the response would drop it from both
	// buckets and let the run read as green while its replacement is unknown.
	// A missing check consumes grace exactly like an unrefreshed one, so it
	// stays awaiting for a bounded number of polls and then becomes unresolved.
	var missing []string
	for name := range b.rollup {
		if present[name] || b.used(name) == 0 {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	order = append(order, missing...)

	for _, name := range order {
		if refreshed[name] || !b.consumeRollupGrace(name) {
			unresolved = append(unresolved, name)
			continue
		}
		awaiting = append(awaiting, name)
	}
	return unresolved, awaiting
}

// cancelledWithoutRerun returns the checks the provider cancelled that this run
// has not spent a rerun on, deduplicated by name.
//
// A cancelled check is terminal: the provider has published a conclusion for it
// and will not publish another one by itself. A rerun is the only thing that
// could replace it, and a check with none spent either had no budget (the
// default) or exhausted it, so there is nothing outstanding to wait for. That
// makes these checks unresolved in the same sense as the ones that came back
// cancelled after their rerun, and they belong at the same gate.
//
// Checks this run DID re-run are deliberately excluded: cancelledAfterRerun
// owns those, because it also has to tell a republished rerun apart from a
// rollup that has not refreshed yet, and it spends rollup grace while doing so.
func (b *checkRerunBudget) cancelledWithoutRerun(checks []scm.Check) []string {
	var names []string
	seen := map[string]struct{}{}
	for _, check := range checks {
		if check.Bucket != scm.CheckBucketCancel || b.used(check.Name) > 0 {
			continue
		}
		if _, ok := seen[check.Name]; ok {
			continue
		}
		seen[check.Name] = struct{}{}
		names = append(names, check.Name)
	}
	return names
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

// awaitingRollupPublication reports whether check is still showing the exact
// outcome its last rerun was requested for, with grace left for the provider to
// publish that rerun.
//
// It only reads the recorded state. cancelledAfterRerun owns spending grace,
// which must happen once per poll rather than once per decision that consults
// it, so every other caller asks this question without answering it.
func (b *checkRerunBudget) awaitingRollupPublication(check scm.Check) bool {
	state, ok := b.rollup[check.Name]
	return ok && state.graceRemaining > 0 && b.rollupUnchanged(check)
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
		// A rerun was already requested for this exact observation and the
		// provider has not published it yet, so the spent-count alone would
		// read a budget of two or more as licence to ask again: the job is
		// already re-running, and the request either bounces or bills the
		// repository a duplicate workflow run. Declining leaves the poll with
		// no candidates, which is what routes the check to cancelledAfterRerun
		// to spend a poll of its grace instead.
		if budget.awaitingRollupPublication(check) {
			continue
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
// loadRerunBudget restores the durable rerun budget for this run. A run that
// never spent one, or a database that cannot be read, leaves the in-memory
// budget as it is: the failure direction is a fresh budget, which the
// reservation write below then re-establishes.
func (s *CIStep) loadRerunBudget(sctx *pipeline.StepContext) {
	if sctx.DB == nil || sctx.Run == nil {
		return
	}
	encoded, err := sctx.DB.GetRunCIRerunState(sctx.Run.ID)
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: could not read the persisted rerun budget: %v", err))
		return
	}
	if err := s.transientReruns.unmarshal(encoded); err != nil {
		sctx.Log(fmt.Sprintf("warning: could not restore the persisted rerun budget: %v", err))
	}
}

// persistRerunBudget writes the rerun budget so a recovered run resumes with
// what it already spent rather than a fresh allowance.
func (s *CIStep) persistRerunBudget(sctx *pipeline.StepContext) error {
	return s.persistRerunBudgetCandidate(sctx, &s.transientReruns)
}

func (s *CIStep) persistRerunBudgetCandidate(sctx *pipeline.StepContext, candidate *checkRerunBudget) error {
	if sctx.DB == nil || sctx.Run == nil {
		return nil
	}
	encoded, err := candidate.marshal()
	if err != nil {
		return err
	}
	return sctx.DB.SetRunCIRerunState(sctx.Run.ID, encoded)
}

func (s *CIStep) retireResolvedReruns(sctx *pipeline.StepContext, checks []scm.Check) (bool, error) {
	return s.transientReruns.retireResolvedReruns(checks, sctx.Run.HeadSHA, func(candidate *checkRerunBudget) error {
		return s.persistRerunBudgetCandidate(sctx, candidate)
	})
}

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
		used := s.transientReruns.spend(check, checks, sctx.Run.HeadSHA)
		// Reserve durably before asking the provider. A crash between this
		// write and the request costs the budget, which is the safe direction:
		// the alternative is a recovered run believing the rerun never
		// happened and issuing another one past the documented limit.
		if err := s.persistRerunBudget(sctx); err != nil {
			sctx.Log(fmt.Sprintf("warning: could not reserve the rerun budget for %s: %v", check.Name, err))
			return issued, nil
		}
		if err := rerunner.RerunCheck(sctx.Ctx, pr, check); err != nil {
			sctx.Log(fmt.Sprintf("warning: could not re-run transient CI check %s: %v", check.Name, err))
			continue
		}
		issued = true
		// used can never exceed limit: selection reserved this rerun against
		// the same shared budget key before it was spent.
		sctx.Log(fmt.Sprintf("re-running CI check %s (%d/%d): %s, not a job failure", check.Name, used, limit, transientReason(check)))
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

// transientReason describes why a check is being re-run rather than escalated,
// so a pre-run infrastructure failure is not logged as the "failure" its
// provider state still reports.
func transientReason(check scm.Check) string {
	if check.PreRunFailure {
		return "it failed before the repository's own steps ran (setup/action resolution)"
	}
	return fmt.Sprintf("provider reported %s", transientStateLabel(check))
}

// markPreRunInfraFailures asks the provider which failed checks it failed before
// any repository step ran and re-buckets those into the transient path: a
// setup/action-resolution outage is not a verdict on the code, so it earns the
// same rerun-then-park treatment as a provider cancellation rather than a fix
// round. It is gated on the transient rerun budget being enabled, so a repo that
// has not opted in pays no extra provider calls and keeps the prior behavior.
//
// It re-buckets a flagged check to the cancel bucket (leaving its provider State
// untouched) so the entire cancel-shaped monitor path - kept out of the fix
// agent, re-run within budget, parked for a decision if it never clears - is
// reused unchanged. classifyCheckFailure reads PreRunFailure, not the bucket, so
// the check still classifies transient despite its FAILURE state.
func markPreRunInfraFailures(sctx *pipeline.StepContext, host scm.Host, checks []scm.Check) {
	if sctx.Config.CI.RerunTransient <= 0 {
		return
	}
	detector, ok := host.(scm.PreRunFailureDetector)
	if !ok {
		return
	}
	var failedIdx []int
	for i := range checks {
		if checks[i].Failing() {
			failedIdx = append(failedIdx, i)
		}
	}
	if len(failedIdx) == 0 {
		return
	}
	failed := make([]scm.Check, len(failedIdx))
	for j, idx := range failedIdx {
		failed[j] = checks[idx]
	}
	infra, err := detector.PreRunFailures(sctx.Ctx, failed)
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: could not classify pre-run CI failures: %v", err))
		return
	}
	if len(infra) != len(failed) {
		sctx.Log(fmt.Sprintf("warning: pre-run CI classifier returned %d results for %d checks; ignoring", len(infra), len(failed)))
		return
	}
	for j, idx := range failedIdx {
		if infra[j] {
			checks[idx].PreRunFailure = true
			checks[idx].Bucket = scm.CheckBucketCancel
		}
	}
}

// ciUnresolvedCancelledOutcome parks the run for transient checks that will not
// resolve on their own: either the run already spent their rerun budget and they
// came back cancelled or failed during setup again, or no rerun is outstanding.
//
// A cancellation is never a verdict on the code, so there is nothing for the fix
// agent to repair: routing it into the auto_fix.ci loop would spend an agent
// round - and let that agent edit code the provider never tested - chasing an
// outcome only the provider can clear. The findings are ask-user for the same
// reason, so a fix loop cannot pick them up later either.
//
// checks preserves the provider-attributed cause through the shared cancel
// bucket so the approval result does not describe a setup failure as a
// cancellation. reruns reports how many reruns this run spent on each check.
func ciUnresolvedCancelledOutcome(names []string, checks []scm.Check, reruns func(string) int) *pipeline.StepOutcome {
	unresolved := unresolvedTransientChecks(names, checks)
	preRunCount := 0
	for _, check := range unresolved {
		if check.PreRunFailure {
			preRunCount++
		}
	}
	findings := Findings{Summary: unresolvedTransientSummary(len(unresolved), preRunCount)}
	for _, check := range unresolved {
		findings.Items = append(findings.Items, Finding{
			Severity:    "warning",
			Description: unresolvedTransientDescription(check.Name, reruns(check.Name), check.PreRunFailure),
			Action:      types.ActionAskUser,
		})
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		Findings:      string(findingsJSON),
	}
}

// unresolvedTransientChecks keeps the cause attached to each provider check.
// Names identify the unresolved budget entries, but they are not unique check
// identities: separate workflows can publish same-named checks with different
// causes. A missing check has no current positional evidence, so it retains the
// historical cancellation diagnosis.
func unresolvedTransientChecks(names []string, checks []scm.Check) []scm.Check {
	var unresolved []scm.Check
	for _, name := range names {
		matched := false
		for _, check := range checks {
			if check.Name != name || check.Bucket != scm.CheckBucketCancel {
				continue
			}
			matched = true
			unresolved = append(unresolved, check)
		}
		if !matched {
			unresolved = append(unresolved, scm.Check{Name: name, Bucket: scm.CheckBucketCancel})
		}
	}
	return unresolved
}

func unresolvedTransientSummary(total, preRun int) string {
	switch {
	case total > 0 && preRun == total:
		return "CI checks failed before repository steps ran"
	case preRun > 0:
		return "CI checks ended without reporting a code verdict"
	default:
		return "CI checks were cancelled without reporting a verdict"
	}
}

func unresolvedTransientDescription(name string, reruns int, preRunFailure bool) string {
	if preRunFailure {
		if reruns > 0 {
			return fmt.Sprintf("CI check failed during setup again after its rerun: %s - repository steps never ran, so it needs a decision rather than a code fix", name)
		}
		return fmt.Sprintf("CI check failed during setup before repository steps ran: %s - no rerun is outstanding to replace that result, so it needs a decision rather than a code fix", name)
	}
	if reruns > 0 {
		return fmt.Sprintf("CI check cancelled again after its rerun: %s - the provider cancelled it rather than reporting a job failure, so it needs a decision rather than a code fix", name)
	}
	return fmt.Sprintf("CI check cancelled without a verdict: %s - the provider cancelled it rather than reporting a job failure, and no rerun is outstanding to replace that result, so it needs a decision rather than a code fix", name)
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
