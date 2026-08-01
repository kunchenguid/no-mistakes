package branchsync

import "testing"

// The synchronization predicate is what presenters use to decide whether an
// action can be offered as the way to take the pipeline's commits, so every
// minted code has to be classified deliberately rather than by string match at
// the call site.
func TestNextActionIsSynchronization(t *testing.T) {
	tests := []struct {
		code string
		want bool
	}{
		{NextActionSync, true},
		{NextActionCheckSync, true},
		{NextActionRecoverCustody, true},
		{NextActionRetry, true},
		{NextActionRunPipeline, false},
		{NextActionInspectWorktree, false},
		{NextActionContinueActiveRun, false},
		{NextActionInspectAndReconcileManually, false},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			action := &NextAction{Code: tt.code}
			if got := action.IsSynchronization(); got != tt.want {
				t.Errorf("IsSynchronization() = %v, want %v", got, tt.want)
			}
		})
	}
	if (*NextAction)(nil).IsSynchronization() {
		t.Error("a missing next action must not read as a synchronization action")
	}
}
