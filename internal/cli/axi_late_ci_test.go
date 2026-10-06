package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAxiLateCIFindingUsesHeadBoundMethodAndRefusesOldDaemon(t *testing.T) {
	for _, old := range []bool{true, false} {
		t.Run(map[bool]string{true: "old-daemon", false: "head-bound-daemon"}[old], func(t *testing.T) {
			socket := filepath.Join(makeSocketSafeTempDir(t), "late.sock")
			server := ipc.NewServer()
			server.Handle(ipc.MethodRespond, func(context.Context, json.RawMessage) (interface{}, error) {
				t.Error("late finding sent as ordinary unbound approval")
				return &ipc.RespondResult{OK: true}, nil
			})
			if !old {
				server.Handle(ipc.MethodRespondLateCI, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
					var params ipc.RespondParams
					if err := json.Unmarshal(raw, &params); err != nil {
						t.Error(err)
					}
					if params.ExpectedHeadSHA != "exact-head" || params.RunID != "run" || len(params.AddedFindings) != 1 {
						t.Errorf("head-bound request lost fields: %+v", params)
					}
					return &ipc.RespondResult{OK: true}, nil
				})
			}
			startIPCServer(t, server, socket)
			client := dialReady(t, socket)
			defer client.Close()
			err := sendLateCIRespond(client, &ipc.RespondParams{RunID: "run", ExpectedHeadSHA: "exact-head", Step: types.StepCI, Action: types.ActionFix, AddedFindings: []types.Finding{{Description: "new requirement"}}})
			if old {
				rpcErr, ok := err.(*ipc.RPCError)
				if !ok || rpcErr.Code != ipc.ErrMethodNotFound {
					t.Fatalf("old daemon accepted amendment: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAxiLateCIRespondCLITransportsAddedFindingInstructions(t *testing.T) {
	for _, mode := range []string{"generated-id", "explicit-id", "embedded-guidance", "global-overrides", "ordinary"} {
		t.Run(mode, func(t *testing.T) {
			var responded atomic.Bool
			requests := make(chan ipc.RespondParams, 1)
			head := strings.Repeat("a", 40)
			olderDaemonFixture(t, nil, func(server *ipc.Server) {
				run := func() *ipc.RunInfo {
					status := types.RunRunning
					if responded.Load() {
						status = types.RunCompleted
					}
					return &ipc.RunInfo{ID: "late-cli", Branch: "feature/omit", HeadSHA: head, Status: status}
				}
				server.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
					return &ipc.GetActiveRunResult{Run: run()}, nil
				})
				server.Handle(ipc.MethodGetRun, func(context.Context, json.RawMessage) (interface{}, error) {
					return &ipc.GetRunResult{Run: run()}, nil
				})
				server.HandleStream(ipc.MethodSubscribe, hangSubscribe)
				for _, method := range []string{ipc.MethodRespondLateCI, ipc.MethodRespond} {
					method := method
					server.Handle(method, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
						wantMethod := ipc.MethodRespondLateCI
						if mode == "ordinary" {
							wantMethod = ipc.MethodRespond
						}
						if method != wantMethod {
							return nil, fmt.Errorf("wrong response method: %s", method)
						}
						var params ipc.RespondParams
						if err := json.Unmarshal(raw, &params); err != nil {
							return nil, err
						}
						requests <- params
						responded.Store(true)
						return &ipc.RespondResult{OK: true}, nil
					})
				}
			})
			finding := types.Finding{Description: "new acceptance condition"}
			if mode != "generated-id" {
				finding.ID = "operator-42"
			}
			if mode == "embedded-guidance" || mode == "global-overrides" {
				finding.UserInstructions = "embedded guidance"
			}
			raw, err := json.Marshal(finding)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"axi", "respond", "--step", "ci", "--action", "fix", "--add-finding", string(raw), "--wait", "3s"}
			if mode != "ordinary" {
				args = append(args, "--head", head)
			}
			if mode != "embedded-guidance" {
				args = append(args, "--instructions", "  preserve parser behavior  ")
			}
			out, err := executeCmd(args...)
			if err != nil {
				t.Fatalf("CLI response failed: %v\n%s", err, out)
			}
			params := <-requests
			if params.RunID != "late-cli" || params.Step != types.StepCI || params.Action != types.ActionFix || len(params.AddedFindings) != 1 || len(params.FindingIDs) != 0 {
				t.Fatalf("CLI request lost amendment fields: %+v", params)
			}
			wantGuidance := "preserve parser behavior"
			wantHead := head
			if mode == "embedded-guidance" {
				wantGuidance = "embedded guidance"
			}
			if mode == "ordinary" {
				wantGuidance, wantHead = "", ""
			}
			got := params.AddedFindings[0]
			if got.ID != finding.ID || got.Description != finding.Description || got.UserInstructions != wantGuidance || params.ExpectedHeadSHA != wantHead {
				t.Fatalf("CLI request lost ID, guidance or head: %+v", params)
			}
		})
	}
}
