package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
)

// The intent-required refusal is the first thing an agent hits when it comes
// back to a branch the pipeline pushed commits to, so the sync action that
// unblocks it must travel in the document rather than only in prose elsewhere.
func TestEmitIntentRequiredError_CarriesBranchSyncNextAction(t *testing.T) {
	state := branchsync.State{
		State: branchsync.StateBehind,
		NextAction: &branchsync.NextAction{
			Code:    branchsync.NextActionSync,
			Command: "no-mistakes axi sync",
		},
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	err := emitIntentRequiredError(cmd, state)

	exit, ok := err.(*exitError)
	if !ok {
		t.Fatalf("error type = %T, want *exitError", err)
	}
	if exit.code != 2 {
		t.Errorf("exit code = %d, want 2", exit.code)
	}
	got := out.String()
	for _, want := range []string{
		"--intent is required to start a run",
		`Pass what the user set out to accomplish: no-mistakes axi run --intent \"the user's goal\"`,
		"branch_sync",
		"state: behind",
		"next_action",
		"no-mistakes axi sync",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("intent-required document missing %q in:\n%s", want, got)
		}
	}
}

// Most next actions are not commands that take the pipeline's commits.
// `run_pipeline` is the very command being refused here, so labelling it as the
// way forward would send the agent in a circle; the code still travels in the
// branch_sync next_action field.
func TestEmitIntentRequiredError_OmitsTakeCommitsHintForNonSyncNextAction(t *testing.T) {
	for _, code := range []branchsync.NextActionCode{
		branchsync.NextActionRunPipeline,
		branchsync.NextActionInspectWorktree,
		branchsync.NextActionContinueActiveRun,
		branchsync.NextActionInspectAndReconcileManually,
	} {
		t.Run(string(code), func(t *testing.T) {
			state := branchsync.State{
				State:      branchsync.StateLocalAhead,
				NextAction: &branchsync.NextAction{Code: code, Command: "no-mistakes axi status"},
			}

			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)

			if err := emitIntentRequiredError(cmd, state); err == nil {
				t.Fatal("intent-required must still fail")
			}
			got := out.String()
			if strings.Contains(got, "Take the pipeline's commits first") {
				t.Errorf("%s must not be labelled as taking the pipeline's commits:\n%s", code, got)
			}
			if !strings.Contains(got, "next_action") || !strings.Contains(got, string(code)) {
				t.Errorf("next_action %s must still travel in the document:\n%s", code, got)
			}
		})
	}
}

// The sync and recovery codes are the ones that genuinely take or re-check the
// pipeline's commits, so those keep the prose hint.
func TestEmitIntentRequiredError_KeepsTakeCommitsHintForSyncCodes(t *testing.T) {
	for _, code := range []branchsync.NextActionCode{
		branchsync.NextActionSync,
		branchsync.NextActionCheckSync,
		branchsync.NextActionRecoverCustody,
		branchsync.NextActionRetry,
	} {
		t.Run(string(code), func(t *testing.T) {
			state := branchsync.State{
				State:      branchsync.StateBehind,
				NextAction: &branchsync.NextAction{Code: code, Command: "no-mistakes axi sync"},
			}

			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)

			if err := emitIntentRequiredError(cmd, state); err == nil {
				t.Fatal("intent-required must still fail")
			}
			if got := out.String(); !strings.Contains(got, "Take the pipeline's commits first") {
				t.Errorf("%s must keep the recovery hint:\n%s", code, got)
			}
		})
	}
}

// With nothing to synchronize there is no action to offer, so the refusal must
// stay the plain one-line error instead of growing an empty recovery hint.
func TestEmitIntentRequiredError_OmitsSyncHintWithoutNextAction(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := emitIntentRequiredError(cmd, branchsync.State{State: branchsync.StateSynchronized}); err == nil {
		t.Fatal("intent-required must still fail")
	}
	if got := out.String(); strings.Contains(got, "next_action") {
		t.Errorf("synchronized branch must offer no next_action:\n%s", got)
	}
}
