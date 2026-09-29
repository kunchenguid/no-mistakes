package daemon

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExternalCIOwnerPushAndRerunRequireExplicitRequest(t *testing.T) {
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	_, head := setupTestGitRepo(t, p, database, "external-ci-owner-repo")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	skips := []types.StepName{types.StepPush, types.StepPR, types.StepCI}
	owner := types.ExternalCIOwnerControllerShipPR
	push := &ipc.PushReceivedParams{Gate: p.RepoDir("external-ci-owner-repo"), Ref: "refs/heads/main", Old: "0000000000000000000000000000000000000000", New: head, SkipSteps: skips, ExternalCIOwner: owner}
	var first ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, push, &first); err != nil {
		t.Fatal(err)
	}
	stored := waitForRunTerminalState(t, database, first.RunID)
	if stored.ExternalCIOwner != owner {
		t.Fatalf("first owner = %q", stored.ExternalCIOwner)
	}
	var status ipc.GetRunResult
	if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: first.RunID}, &status); err != nil {
		t.Fatal(err)
	}
	if status.Run == nil || status.Run.ExternalCIOwner != owner {
		t.Fatalf("IPC owner = %+v", status.Run)
	}
	var ordinary ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: "external-ci-owner-repo", Branch: "main", PreviousRunID: first.RunID}, &ordinary); err != nil {
		t.Fatal(err)
	}
	withoutOwner := waitForRunTerminalState(t, database, ordinary.RunID)
	if withoutOwner.ExternalCIOwner != "" {
		t.Fatalf("rerun silently inherited owner %q", withoutOwner.ExternalCIOwner)
	}
	var invalid ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: "external-ci-owner-repo", Branch: "main", ExternalCIOwner: owner}, &invalid); err == nil {
		t.Fatal("owner without explicit skips accepted")
	}
	var explicit ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: "external-ci-owner-repo", Branch: "main", ExternalCIOwner: owner, SkipSteps: skips}, &explicit); err != nil {
		t.Fatal(err)
	}
	withOwner := waitForRunTerminalState(t, database, explicit.RunID)
	if withOwner.ExternalCIOwner != owner {
		t.Fatalf("explicit rerun owner = %q", withOwner.ExternalCIOwner)
	}
}
