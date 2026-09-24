package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// A workflow held for maintainer approval has run nothing and will run nothing
// until a human acts, so the monitor must name the hold instead of reporting
// checks that are running.
func TestCIWaitingMessage_NamesTheMaintainerApprovalHold(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		checks []scm.Check
		want   string
	}{
		{
			name:   "held workflow",
			checks: []scm.Check{{Name: "CI", Bucket: scm.CheckBucketPending, State: "ACTION_REQUIRED", AwaitingApproval: true}},
			want:   cimonitor.ChecksAwaitingApprovalMsg,
		},
		{
			name:   "held workflow alongside a running check",
			checks: []scm.Check{{Name: "docs", Bucket: scm.CheckBucketPending, State: "IN_PROGRESS"}, {Name: "CI", Bucket: scm.CheckBucketPending, State: "ACTION_REQUIRED", AwaitingApproval: true}},
			want:   cimonitor.ChecksAwaitingApprovalMsg,
		},
		{
			name:   "ordinary running check",
			checks: []scm.Check{{Name: "CI", Bucket: scm.CheckBucketPending, State: "IN_PROGRESS"}},
			want:   cimonitor.ChecksRunningMsg,
		},
		{
			name:   "no checks",
			checks: nil,
			want:   cimonitor.ChecksRunningMsg,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ciWaitingMessage(tc.checks); got != tc.want {
				t.Fatalf("ciWaitingMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The hold is a wait, not a failure: it never reaches the failing set that the
// CI step escalates to a fix round.
func TestHeldCheckIsNeitherFailingNorTerminal(t *testing.T) {
	t.Parallel()

	held := scm.Check{Name: "CI", Bucket: scm.CheckBucketPending, State: "ACTION_REQUIRED", AwaitingApproval: true}
	checks := []scm.Check{held}
	if hasFailingChecks(checks) {
		t.Error("a held workflow must not count as a failing check")
	}
	if names := failingCheckNames(checks); len(names) != 0 {
		t.Errorf("failingCheckNames() = %v, want none", names)
	}
	if checkFailedTerminally(held) {
		t.Error("a held workflow has not failed terminally")
	}
	if !hasPendingChecks(checks) {
		t.Error("a held workflow must keep the PR pending")
	}
	if allChecksPassed(checks) {
		t.Error("a held workflow must never read as passed")
	}
	// It is not a terminal failure, so it is never classified as one either -
	// the classes that decide a rerun against a fix round are for checks that
	// actually failed.
	if got := classifyCheckFailure(held); got != classUnknown {
		t.Errorf("classifyCheckFailure(held) = %q, want %q", got, classUnknown)
	}
	// A genuine failure is unchanged, including one GitHub concluded
	// ACTION_REQUIRED after running jobs.
	genuine := scm.Check{Name: "CI", Bucket: scm.CheckBucketFail, State: "ACTION_REQUIRED"}
	if !hasFailingChecks([]scm.Check{genuine}) {
		t.Error("an action_required check that ran jobs must still count as failing")
	}
	if got := classifyCheckFailure(genuine); got != classGenuine {
		t.Errorf("classifyCheckFailure(genuine) = %q, want %q", got, classGenuine)
	}
}
