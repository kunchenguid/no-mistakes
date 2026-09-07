package steps

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestAllChecksPassedFailsClosed(t *testing.T) {
	tests := []struct {
		name  string
		check scm.Check
		ready bool
	}{
		{name: "pass", check: scm.Check{Bucket: scm.CheckBucketPass}, ready: true},
		{name: "skip", check: scm.Check{Bucket: scm.CheckBucketSkip}, ready: true},
		{name: "pending", check: scm.Check{Bucket: scm.CheckBucketPending}},
		{name: "failure", check: scm.Check{Bucket: scm.CheckBucketFail}},
		{name: "cancel", check: scm.Check{Bucket: scm.CheckBucketCancel}},
		{name: "unknown", check: scm.Check{}, ready: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := []scm.Check{tt.check}
			if got := allChecksPassed(checks); got != tt.ready {
				t.Fatalf("allChecksPassed() = %v, want %v", got, tt.ready)
			}
			if !tt.ready && !hasUnresolvedChecks(checks) && tt.check.Bucket != scm.CheckBucketFail {
				t.Fatal("non-ready check must be unresolved or failing")
			}
		})
	}
	if allChecksPassed(nil) {
		t.Fatal("empty checks must not pass")
	}
}

func TestCITimeoutFindingsPreserveSameNamedCheckIdentity(t *testing.T) {
	t.Parallel()

	checks := []scm.Check{
		{Name: "build", ProviderID: "github-check-run:41", Bucket: scm.CheckBucketFail},
		{Name: "build", ProviderID: "github-check-run:42", Bucket: scm.CheckBucketFail},
		{Name: "deploy", ProviderID: "github-check-run:43", Bucket: scm.CheckBucketPending},
	}
	targets := terminalCheckTargetsForNames(checks, []string{"build"})
	outcome := ciFailureOutcome(targets, false, "timed out")
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 2 || findings.Items[0].CheckID != "github-check-run:41" || findings.Items[1].CheckID != "github-check-run:42" {
		t.Fatalf("timeout findings = %+v, want exact identities for both same-named failures", findings.Items)
	}
}

func TestPendingCheckMatchesLastFixed_SpecialCheckNames(t *testing.T) {
	t.Parallel()

	lastFixedChecks := encodeLastFixedChecks([]scm.CheckTarget{{Name: "lint,unit"}, {Name: "deploy+conflict"}}, true)
	checks := []scm.Check{
		{Name: "lint,unit", Bucket: "pending"},
	}

	if !pendingCheckMatchesLastFixed(checks, lastFixedChecks) {
		t.Fatalf("expected pending check with special characters to match encoded last fixed checks %q", lastFixedChecks)
	}

	checks = []scm.Check{
		{Name: "lint", Bucket: "pending"},
	}
	if pendingCheckMatchesLastFixed(checks, lastFixedChecks) {
		t.Fatalf("expected unrelated pending check not to match encoded last fixed checks %q", lastFixedChecks)
	}
}

func TestLastFixedTrackingUsesProviderIdentityForSameNamedChecks(t *testing.T) {
	t.Parallel()
	completed := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	code := scm.Check{Name: "build", ProviderID: "github-check-run:41", Bucket: scm.CheckBucketFail, CompletedAt: completed}
	bot := scm.Check{Name: "build", ProviderID: "github-check-run:42", Bucket: scm.CheckBucketFail, CompletedAt: completed}
	step := &CIStep{
		lastFixedChecks:      encodeLastFixedChecks([]scm.CheckTarget{{Name: code.Name, ProviderID: code.ProviderID}}, false),
		lastFixedCompletedAt: terminalFailureCompletionTimes([]scm.Check{code, bot}),
	}

	if step.lastRepairStillUnverified([]scm.Check{bot}, false) {
		t.Fatal("same-named bot failure must not stand in for the repaired check")
	}
	bot.Bucket = scm.CheckBucketPending
	if pendingCheckMatchesLastFixed([]scm.Check{bot}, step.lastFixedChecks) {
		t.Fatal("same-named bot pending state must not clear the repaired check tracker")
	}
	code.Bucket = scm.CheckBucketPending
	if !pendingCheckMatchesLastFixed([]scm.Check{code}, step.lastFixedChecks) {
		t.Fatal("the repaired check's pending state must clear its tracker")
	}
	bot.Bucket = scm.CheckBucketFail
	bot.CompletedAt = completed.Add(time.Minute)
	selectedCompletions := completionTimesForTargets(terminalFailureCompletionTimes([]scm.Check{code, bot}), []scm.CheckTarget{{Name: code.Name, ProviderID: code.ProviderID}})
	if terminalFailureCompletedAfter([]scm.Check{bot}, selectedCompletions) {
		t.Fatal("same-named bot completion must not look like the repaired check reran")
	}
}

// A cancelled check can be a fix target, so the completion snapshot that lets
// the step notice its own CI re-run has to cover it. Keyed on the fail bucket
// alone, a cancelled-only fix round records nothing and the step can only log
// "fix already attempted" until its idle timeout.
func TestTerminalFailureCompletionTimesCoverCancelledChecks(t *testing.T) {
	t.Parallel()

	completed := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	cancelled := scm.Check{Name: "build", Bucket: scm.CheckBucketCancel, State: "CANCELLED", CompletedAt: completed}

	before := terminalFailureCompletionTimes([]scm.Check{cancelled})
	if got, ok := before["build"]; !ok || !got.CompletedAt.Equal(completed) {
		t.Fatalf("completion times = %v, want the cancelled check recorded at %v", before, completed)
	}

	if terminalFailureCompletedAfter([]scm.Check{cancelled}, before) {
		t.Fatal("the same observation must not read as a re-run")
	}

	rerun := cancelled
	rerun.CompletedAt = completed.Add(2 * time.Minute)
	if !terminalFailureCompletedAfter([]scm.Check{rerun}, before) {
		t.Fatal("a cancelled check that completed again after the fix push must read as a re-run")
	}
}

func TestTerminalFailureFreshnessUsesExecutionIDWithoutCompletionTime(t *testing.T) {
	t.Parallel()

	before := terminalFailureCompletionTimes([]scm.Check{{Name: "build", ProviderID: "bitbucket-status:build", ExecutionID: "41", Bucket: scm.CheckBucketFail}})
	if terminalFailureCompletedAfter([]scm.Check{{Name: "build", ProviderID: "bitbucket-status:build", ExecutionID: "41", Bucket: scm.CheckBucketFail}}, before) {
		t.Fatal("the same provider execution must not read as a rerun")
	}
	if !terminalFailureCompletedAfter([]scm.Check{{Name: "build", ProviderID: "bitbucket-status:build", ExecutionID: "42", Bucket: scm.CheckBucketFail}}, before) {
		t.Fatal("a new provider execution must read as a rerun without a completion time")
	}
}

// The fail bucket keeps the behavior it always had.
func TestTerminalFailureCompletionTimesStillCoverFailingChecks(t *testing.T) {
	t.Parallel()

	completed := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	failing := scm.Check{Name: "lint", Bucket: scm.CheckBucketFail, State: "FAILURE", CompletedAt: completed}

	before := terminalFailureCompletionTimes([]scm.Check{failing})
	if got, ok := before["lint"]; !ok || !got.CompletedAt.Equal(completed) {
		t.Fatalf("completion times = %v, want the failing check recorded at %v", before, completed)
	}

	rerun := failing
	rerun.CompletedAt = completed.Add(time.Minute)
	if !terminalFailureCompletedAfter([]scm.Check{rerun}, before) {
		t.Fatal("a failing check that completed again after the fix push must read as a re-run")
	}

	// Passing and skipped checks are not failures and must stay out of the
	// snapshot, or an unrelated green check would reset the fix bookkeeping.
	quiet := terminalFailureCompletionTimes([]scm.Check{
		{Name: "docs", Bucket: scm.CheckBucketPass, State: "SUCCESS", CompletedAt: completed},
		{Name: "flaky", Bucket: scm.CheckBucketSkip, State: "SKIPPED", CompletedAt: completed},
	})
	if quiet != nil {
		t.Fatalf("completion times = %v, want nothing recorded for non-failures", quiet)
	}
}
