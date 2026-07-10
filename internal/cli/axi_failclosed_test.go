package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestDriveRunUnattendedFailsClosedOnUnresolvedBlocking proves criterion 4:
// under --yes, reaching a gate while a blocking lineage is unresolved aborts and
// fails rather than approving or re-fixing.
func TestDriveRunUnattendedFailsClosedOnUnresolvedBlocking(t *testing.T) {
	dir, err := os.MkdirTemp("", "cli-ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")

	awaiting := &ipc.RunInfo{
		ID:     "run-1",
		RepoID: "repo-1",
		Branch: "feature",
		Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{{
			ID:       "s1",
			RunID:    "run-1",
			StepName: types.StepReview,
			Status:   types.StepStatusAwaitingApproval,
		}},
	}
	var respondedAction types.ApprovalAction
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodGetRun, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return &ipc.GetRunResult{Run: awaiting}, nil
	})
	srv.Handle(ipc.MethodRespond, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var p ipc.RespondParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		respondedAction = p.Action
		return &ipc.RespondResult{OK: true}, nil
	})
	go func() { _ = srv.Serve(sock) }()
	t.Cleanup(srv.Close)

	client := dialWithRetry(t, sock)
	defer client.Close()

	blockingUnresolved := func(string) (bool, error) { return true, nil }
	_, _, err = driveRun(context.Background(), io.Discard, client, "run-1", true, nil, blockingUnresolved)
	if err == nil {
		t.Fatal("expected fail-closed error when a blocking lineage is unresolved under --yes")
	}
	if respondedAction != types.ActionAbort {
		t.Fatalf("unattended consent responded %q, want abort", respondedAction)
	}
}

// TestDriveRunUnattendedProceedsWhenResolved shows the drive resolves the gate
// normally when no blocking lineage is unresolved.
func TestDriveRunUnattendedProceedsWhenResolved(t *testing.T) {
	dir, err := os.MkdirTemp("", "cli-ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")

	run := &ipc.RunInfo{
		ID: "run-2", RepoID: "repo-1", Branch: "feature", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{{ID: "s1", RunID: "run-2", StepName: types.StepReview, Status: types.StepStatusAwaitingApproval}},
	}
	responded := false
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodGetRun, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		// Once a response arrives, report the run completed so the drive ends.
		if responded {
			done := *run
			done.Status = types.RunCompleted
			done.Steps = []ipc.StepResultInfo{{ID: "s1", RunID: "run-2", StepName: types.StepReview, Status: types.StepStatusCompleted}}
			return &ipc.GetRunResult{Run: &done}, nil
		}
		return &ipc.GetRunResult{Run: run}, nil
	})
	srv.Handle(ipc.MethodRespond, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		responded = true
		return &ipc.RespondResult{OK: true}, nil
	})
	go func() { _ = srv.Serve(sock) }()
	t.Cleanup(srv.Close)

	client := dialWithRetry(t, sock)
	defer client.Close()

	blockingUnresolved := func(string) (bool, error) { return false, nil }
	final, _, err := driveRun(context.Background(), io.Discard, client, "run-2", true, nil, blockingUnresolved)
	if err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if final == nil || final.Status != types.RunCompleted {
		t.Fatalf("final run = %+v, want completed", final)
	}
}

func dialWithRetry(t *testing.T, sock string) *ipc.Client {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		client, err := ipc.Dial(sock)
		if err == nil {
			return client
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %s: %v", sock, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
