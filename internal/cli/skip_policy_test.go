package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiRunSkipRefusesLegacyAndMismatchedAttachment(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "mismatch"}[known], func(t *testing.T) {
			var responses atomic.Bool
			fx := newAxiTimeoutFixture(t, axiTimeoutOpts{responded: &responses})
			fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
				r := fx.awaiting()
				r.SkipPolicyKnown = known
				return r, nil
			})
			out, err := executeCmd("axi", "run", "--skip", "push,pr,ci", "--yes", "--wait", "3s")
			if err == nil || !strings.Contains(out, "skip policy") {
				t.Fatalf("err=%v output=%s", err, out)
			}
			if responses.Load() {
				t.Fatal("scope mismatch drove the run")
			}
		})
	}
}

func TestAxiRespondRequireSkipRefusesBeforeUnscopedResponse(t *testing.T) {
	for _, testCase := range []string{"legacy", "mismatch", "old-daemon"} {
		t.Run(testCase, func(t *testing.T) {
			var unscoped atomic.Bool
			fx := newAxiTimeoutFixture(t, axiTimeoutOpts{respond: func(context.Context, json.RawMessage) (interface{}, error) {
				unscoped.Store(true)
				return &ipc.RespondResult{OK: true}, nil
			}})
			fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
				r := fx.awaiting()
				r.SkipPolicyKnown = testCase != "legacy"
				if testCase == "old-daemon" {
					r.SkipSteps = []types.StepName{types.StepPush, types.StepPR, types.StepCI}
				}
				return r, nil
			})
			out, err := executeCmd("axi", "respond", "--run", "run-timeout", "--require-skip", "push,pr,ci", "--action", "approve", "--wait", "3s")
			if err == nil {
				t.Fatalf("unsafe response accepted: %s", out)
			}
			if unscoped.Load() {
				t.Fatal("scoped caller fell back to unscoped response")
			}
			if testCase != "old-daemon" && !strings.Contains(out, "skip policy") {
				t.Fatalf("wrong refusal: %s", out)
			}
			if testCase == "old-daemon" && !strings.Contains(out, "respond_scoped") {
				t.Fatalf("missing capability refusal: %s", out)
			}
		})
	}
}

func TestAxiRespondRequireSkipUsesScopedRPCAndExactRun(t *testing.T) {
	var called atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{scopedRespond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var p ipc.ScopedRespondParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if p.RunID != "run-timeout" || len(p.RequiredSkips) != 3 {
			t.Errorf("wrong scope: %+v", p)
		}
		called.Store(true)
		return &ipc.RespondResult{OK: true}, nil
	}})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		r := fx.awaiting()
		if called.Load() {
			r = fx.completed()
		}
		r.SkipPolicyKnown = true
		r.SkipSteps = []types.StepName{types.StepPush, types.StepPR, types.StepCI}
		return r, nil
	})
	out, err := executeCmd("axi", "respond", "--run", "run-timeout", "--require-skip", "push,pr,ci", "--action", "approve", "--wait", "3s")
	if err != nil || !called.Load() {
		t.Fatalf("scoped response: %v %s", err, out)
	}
}
