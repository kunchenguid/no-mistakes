package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A step's next round can park at the same status before any read observes it
// running, so the status alone cannot say whether the gate just answered is
// gone. Every round is persisted before the step parks again, so the round
// count does.
func TestWaitStepLeavesGate_SameStatusAtAHigherRoundIsANewGate(t *testing.T) {
	var round atomic.Int32
	round.Store(1)
	socketPath := filepath.Join(makeSocketSafeTempDir(t), "gate-identity.sock")
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodGetRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetRunResult{Run: &ipc.RunInfo{
			ID:     "run-timeout",
			Status: types.RunRunning,
			Steps: []ipc.StepResultInfo{{
				StepName:   types.StepReview,
				Status:     types.StepStatusAwaitingApproval,
				RoundCount: int(round.Load()),
			}},
		}}, nil
	})
	srv.HandleStream(ipc.MethodSubscribe, tickingSubscribe)
	startIPCServer(t, srv, socketPath)

	answered := gateIdentity{status: string(types.StepStatusAwaitingApproval), round: 1}
	wait := func(ctx context.Context) error {
		return waitStepLeavesGate(ctx, socketPath, "run-timeout", string(types.StepReview), answered)
	}

	stuck, cancelStuck := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancelStuck()
	if err := wait(stuck); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait at the answered gate's own round = %v, want to keep waiting", err)
	}

	round.Store(2)
	moved, cancelMoved := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelMoved()
	if err := wait(moved); err != nil {
		t.Fatalf("re-park at the same status one round later: %v", err)
	}
}

// axi respond hits the same shape: the next round can park before the wait's
// first read, and keyed on status alone that park read as the one just
// responded to, hanging the command until --wait elapsed.
func TestAxiRespond_ReparkAtTheSameStatusIsReturnedAsTheNextGate(t *testing.T) {
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{responded: &responded})
	repark := findingsJSON(t, []types.Finding{reviewCodeFinding()}, "review")
	fx.setGetRun(func(_ context.Context, n int) (*ipc.RunInfo, error) {
		run := fx.running()
		step := ipc.StepResultInfo{
			StepName:   types.StepReview,
			Status:     types.StepStatusAwaitingApproval,
			RoundCount: 2,
		}
		if n == 1 {
			// The gate being responded to.
			step.RoundCount = 1
		} else {
			step.FindingsJSON = &repark
		}
		run.Steps = []ipc.StepResultInfo{step}
		return run, nil
	})

	started := time.Now()
	out, err := executeCmd("axi", "respond", "--action", "approve", "--wait", "6s")
	if err != nil {
		t.Fatalf("axi respond: %v\n%s", err, out)
	}
	if !responded.Load() {
		t.Fatalf("response was not submitted:\n%s", out)
	}
	for _, want := range []string{"gate:", "review-1", "calls os.Exit"} {
		if !strings.Contains(out, want) {
			t.Fatalf("re-park at the same status was not returned as the next gate (missing %q):\n%s", want, out)
		}
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("respond waited %s for a gate that was already parked", elapsed)
	}
}
