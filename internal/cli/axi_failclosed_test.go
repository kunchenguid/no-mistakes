package cli

import (
	"context"
	"encoding/json"
	"errors"
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

func TestDriveRunUnattendedFailsClosedWhenRepairStateIsUnknown(t *testing.T) {
	dir, err := os.MkdirTemp("", "cli-ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")

	awaiting := &ipc.RunInfo{
		ID: "run-unknown", RepoID: "repo-1", Branch: "feature", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{{ID: "s1", RunID: "run-unknown", StepName: types.StepReview, Status: types.StepStatusAwaitingApproval}},
	}
	var responses int
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodGetRun, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return &ipc.GetRunResult{Run: awaiting}, nil
	})
	srv.Handle(ipc.MethodRespond, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		responses++
		return &ipc.RespondResult{OK: true}, nil
	})
	go func() { _ = srv.Serve(sock) }()
	t.Cleanup(srv.Close)

	client := dialWithRetry(t, sock)
	defer client.Close()

	checkErr := errors.New("repair journal unavailable")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err = driveRun(ctx, io.Discard, client, awaiting.ID, true, nil,
		func(string) (bool, error) { return false, checkErr })
	if !errors.Is(err, checkErr) {
		t.Fatalf("driveRun error = %v, want repair checker error %v", err, checkErr)
	}
	if responses != 0 {
		t.Fatalf("respond calls = %d, want none when repair state is unknown", responses)
	}
}

// TestDriveRunUnattendedProceedsWhenResolved shows the drive resolves the gate
// normally when no blocking lineage is unresolved and sends only actionable
// finding IDs to the executor-facing response boundary.
func TestDriveRunUnattendedProceedsWhenResolved(t *testing.T) {
	dir, err := os.MkdirTemp("", "cli-ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")

	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"fixable","action":"auto-fix"},{"id":"review-2","severity":"warning","description":"needs consent","action":"ask-user"},{"id":"review-3","severity":"info","description":"context only","action":"no-op"}],"summary":"3 findings"}`
	run := &ipc.RunInfo{
		ID: "run-2", RepoID: "repo-1", Branch: "feature", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{{ID: "s1", RunID: "run-2", StepName: types.StepReview, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings}},
	}
	responded := false
	var response ipc.RespondParams
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
	srv.Handle(ipc.MethodRespond, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		if err := json.Unmarshal(raw, &response); err != nil {
			return nil, err
		}
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
	if response.Action != types.ActionFix {
		t.Fatalf("response action = %s, want %s", response.Action, types.ActionFix)
	}
	if got := response.FindingIDs; len(got) != 2 || got[0] != "review-1" || got[1] != "review-2" {
		t.Fatalf("response finding IDs = %v, want actionable IDs [review-1 review-2]", got)
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
