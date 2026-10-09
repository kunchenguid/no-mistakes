package cimonitor

import "testing"

// The awaiting-approval line is monitor vocabulary the TUI and axi read back, so
// it must be recognized - and it is never a ready state.
func TestParseActivityRecognizesAwaitingApproval(t *testing.T) {
	t.Parallel()

	logs := []string{ChecksPassedMsg, ChecksAwaitingApprovalMsg}
	activity := ParseActivity(logs)
	if activity.Ready || activity.DeclaredNoCI {
		t.Errorf("activity = %+v, want not ready", activity)
	}
	if activity.LastEvent != ChecksAwaitingApprovalMsg {
		t.Errorf("LastEvent = %q, want the awaiting-approval message", activity.LastEvent)
	}
	if ChecksPassed(logs) {
		t.Error("a held workflow must never report checks passed")
	}
}
