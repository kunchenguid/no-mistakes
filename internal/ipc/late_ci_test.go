package ipc_test

import (
	"context"
	"encoding/json"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"testing"
)

func TestLateCIFindingRefusesOldDaemonInsteadOfUnboundRespond(t *testing.T) {
	sock := socketPath(t)
	server := startServer(t, sock)
	// Older daemons only own ordinary respond, which ignores unknown JSON fields.
	server.Handle(ipc.MethodRespond, func(context.Context, json.RawMessage) (interface{}, error) {
		t.Error("late request downgraded to unbound approval")
		return &ipc.RespondResult{OK: true}, nil
	})
	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	err = client.Call(ipc.MethodRespondLateCI, &ipc.RespondParams{RunID: "run", Step: types.StepCI, Action: types.ActionFix, ExpectedHeadSHA: "exact-head", AddedFindings: []types.Finding{{Description: "new requirement"}}}, nil)
	rpcErr, ok := err.(*ipc.RPCError)
	if !ok || rpcErr.Code != ipc.ErrMethodNotFound {
		t.Fatalf("old daemon did not refuse new method: %v", err)
	}
}
