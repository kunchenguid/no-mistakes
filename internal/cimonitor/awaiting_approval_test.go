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

// The blocking-manual-job line is the same kind of vocabulary: an earlier
// passed-checks line must not survive it, so a pipeline stopped on a person
// never reports ready.
func TestParseActivityRecognizesAwaitingManualJob(t *testing.T) {
	t.Parallel()

	logs := []string{ChecksPassedMsg, ChecksAwaitingManualJobMsg}
	activity := ParseActivity(logs)
	if activity.Ready || activity.DeclaredNoCI {
		t.Errorf("activity = %+v, want not ready", activity)
	}
	if activity.LastEvent != ChecksAwaitingManualJobMsg {
		t.Errorf("LastEvent = %q, want the awaiting-manual-job message", activity.LastEvent)
	}
	if ChecksPassed(logs) {
		t.Error("a pipeline held on a manual job must never report checks passed")
	}
}
