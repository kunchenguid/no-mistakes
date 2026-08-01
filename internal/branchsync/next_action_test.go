package branchsync

import "testing"

// The codes ship verbatim to agents in skills/no-mistakes/sync-recovery.md and
// in docs/src/content/docs/reference/cli.md, so renaming one breaks every agent
// and document matching on it. Pin the exact wire strings here; the rest of the
// package may then use the constants freely.
func TestNextActionCodeWireValues(t *testing.T) {
	want := map[NextActionCode]string{
		NextActionSync:                        "sync",
		NextActionCheckSync:                   "check_sync",
		NextActionRecoverCustody:              "recover_custody",
		NextActionRetry:                       "retry",
		NextActionRunPipeline:                 "run_pipeline",
		NextActionInspectWorktree:             "inspect_worktree",
		NextActionContinueActiveRun:           "continue_active_run",
		NextActionInspectAndReconcileManually: "inspect_and_reconcile_manually",
	}
	for _, code := range allNextActionCodes {
		wire, ok := want[code]
		if !ok {
			t.Errorf("code %q has no pinned wire value; documented codes are a compatibility surface", code)
			continue
		}
		if string(code) != wire {
			t.Errorf("code = %q, want %q", string(code), wire)
		}
	}
	if len(want) != len(allNextActionCodes) {
		t.Errorf("pinned %d wire values for %d codes", len(want), len(allNextActionCodes))
	}
}

// The synchronization predicate is what presenters use to decide whether an
// action can be offered as the way to take the pipeline's commits, so every
// minted code has to be classified deliberately rather than by string match at
// the call site. The table is checked against the complete vocabulary so a new
// code fails here instead of silently defaulting to false.
func TestNextActionIsSynchronization(t *testing.T) {
	want := map[NextActionCode]bool{
		NextActionSync:                        true,
		NextActionCheckSync:                   true,
		NextActionRecoverCustody:              true,
		NextActionRetry:                       true,
		NextActionRunPipeline:                 false,
		NextActionInspectWorktree:             false,
		NextActionContinueActiveRun:           false,
		NextActionInspectAndReconcileManually: false,
	}
	if len(want) != len(allNextActionCodes) {
		t.Fatalf("classified %d codes, want all %d", len(want), len(allNextActionCodes))
	}
	for _, code := range allNextActionCodes {
		expected, ok := want[code]
		if !ok {
			t.Errorf("code %q is unclassified; decide whether it takes the pipeline's commits", code)
			continue
		}
		t.Run(string(code), func(t *testing.T) {
			action := &NextAction{Code: code}
			if got := action.IsSynchronization(); got != expected {
				t.Errorf("IsSynchronization() = %v, want %v", got, expected)
			}
		})
	}
	if (*NextAction)(nil).IsSynchronization() {
		t.Error("a missing next action must not read as a synchronization action")
	}
}

// Each code carries exactly one command, and the constructors are the single
// home for that pairing.
func TestNextActionConstructorsPairCodeWithCommand(t *testing.T) {
	tests := []struct {
		action  *NextAction
		code    NextActionCode
		command string
	}{
		{syncAction(), NextActionSync, "no-mistakes axi sync"},
		{checkSyncAction(), NextActionCheckSync, "no-mistakes axi sync --check"},
		{recoverCustodyAction(), NextActionRecoverCustody, "no-mistakes axi sync --recover"},
		{retryAction(), NextActionRetry, "no-mistakes axi sync --check"},
		{runPipelineAction(), NextActionRunPipeline, `no-mistakes axi run --intent "<what the user set out to accomplish>"`},
		{inspectWorktreeAction(), NextActionInspectWorktree, "git status"},
		{continueActiveRunAction(), NextActionContinueActiveRun, "no-mistakes axi status"},
		{reconcileManuallyAction("refs/no-mistakes/x"), NextActionInspectAndReconcileManually, "git log --oneline --left-right HEAD...refs/no-mistakes/x"},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			if tt.action.Code != tt.code {
				t.Errorf("Code = %q, want %q", tt.action.Code, tt.code)
			}
			if tt.action.Command != tt.command {
				t.Errorf("Command = %q, want %q", tt.action.Command, tt.command)
			}
		})
	}
}
