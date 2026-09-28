package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiRespond_ProviderRecheck(t *testing.T) {
	for _, green := range []bool{false, true} {
		t.Run(fmt.Sprint(green), func(t *testing.T) {
			var calls atomic.Int32
			var completed atomic.Bool
			fx := newAxiTimeoutFixture(t, axiTimeoutOpts{respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
				var params ipc.RespondParams
				if err := json.Unmarshal(raw, &params); err != nil {
					return nil, err
				}
				calls.Add(1)
				if params.Action != types.ActionRecheck || params.Step != types.StepCI || len(params.FindingIDs) != 0 || params.ApprovalReason != "" {
					return nil, fmt.Errorf("unexpected response: %+v", params)
				}
				if !green {
					return nil, fmt.Errorf("CI recheck left the gate unresolved: provider unreadable")
				}
				completed.Store(true)
				return &ipc.RespondResult{OK: true}, nil
			}})
			fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
				if completed.Load() {
					return fx.completed(), nil
				}
				run := fx.awaiting()
				run.Steps[0].StepName = types.StepCI
				return run, nil
			})
			out, err := executeCmd("axi", "respond", "--action", "recheck", "--wait", "3s")
			if calls.Load() != 1 {
				t.Fatalf("responses=%d output=%s error=%v", calls.Load(), out, err)
			}
			if green {
				if err != nil || !strings.Contains(out, "outcome: passed") || strings.Contains(out, "passed-with-override") {
					t.Fatalf("not clean completion: %s %v", out, err)
				}
			} else if err == nil || !strings.Contains(out, "left the gate unresolved") || completed.Load() {
				t.Fatalf("unverified response succeeded: %s %v", out, err)
			}
		})
	}
}

func TestAxiRespond_RecheckWaitIsBoundedWithoutResubmission(t *testing.T) {
	var calls atomic.Int32
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{respond: func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
		calls.Add(1)
		if err := sleepOrDone(ctx, 5*time.Second); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("provider still unreadable")
	}})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		run := fx.awaiting()
		run.Steps[0].StepName = types.StepCI
		return run, nil
	})
	out, err := executeCmd("axi", "respond", "--action", "recheck", "--wait", "3s")
	assertWaitElapsed(t, err, out, "3s")
	if calls.Load() != 1 || !strings.Contains(out, "Re-run `no-mistakes axi run`") {
		t.Fatalf("recheck did not give a non-mutating reattachment: calls=%d %s", calls.Load(), out)
	}
}

func TestAxiRespond_RecheckRejectsWritableOrWaiverInputs(t *testing.T) {
	for _, extra := range [][]string{{"--yes"}, {"--findings", "ci-1"}, {"--instructions", "fix it"}, {"--add-finding", `{"description":"fix"}`}, {"--reason", "waive"}, {"--step", "review"}} {
		out, err := executeCmd(append([]string{"axi", "respond", "--action", "recheck"}, extra...)...)
		if err == nil || !strings.Contains(out, "recheck is CI-only") {
			t.Fatalf("accepted %v: %s %v", extra, out, err)
		}
	}
}

func TestCIProviderGateLeadsWithRecheck(t *testing.T) {
	gate := stepView{Name: "ci", Status: string(types.StepStatusAwaitingApproval), FindingsJSON: `{"findings":[{"id":"ci-1","category":"ci-provider-read","description":"read failed","action":"ask-user"}]}`}
	out := axiDoc(gateFields(gate)...)
	if recheck, approve := strings.Index(out, "--action recheck"), strings.Index(out, "--action approve"); recheck < 0 || recheck > approve {
		t.Fatalf("provider-only recheck not offered first: %s", out)
	}
	inspection := axiDoc(inspectionOnlyGateFields(gate, "other-run")...)
	if strings.Contains(inspection, "--action recheck") {
		t.Fatalf("explicit run selection offered a mutating response: %s", inspection)
	}
}
