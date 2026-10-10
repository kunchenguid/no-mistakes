package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestAxiRespond_RunFlagAnswersTheNamedRunFromOutsideItsWorktree(t *testing.T) {
	var gotParams ipc.RespondParams
	var responded, lookedUpActive atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			if err := json.Unmarshal(raw, &gotParams); err != nil {
				return nil, err
			}
			responded.Store(true)
			return &ipc.RespondResult{OK: true, Fixed: []string{"R1"}, Ignored: []string{"R2"}}, nil
		},
	})
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) {
		lookedUpActive.Store(true)
		other := fx.awaiting()
		other.ID = "run-other"
		return other, nil
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		if responded.Load() {
			return fx.completed(), nil
		}
		return fx.awaiting(), nil
	})
	chdir(t, t.TempDir())

	out, err := executeCmd("axi", "respond", "--run", "run-timeout", "--action", "fix", "--findings", "R1", "--ignore", "R2", "--wait", "3s")
	if err != nil {
		t.Fatalf("respond --run: %v\n%s", err, out)
	}
	if gotParams.RunID != "run-timeout" {
		t.Fatalf("responded to run %q, want run-timeout", gotParams.RunID)
	}
	if strings.Join(gotParams.FindingIDs, ",") != "R1" || strings.Join(gotParams.IgnoreFindingIDs, ",") != "R2" {
		t.Fatalf("forwarded findings=%v ignores=%v, want [R1] [R2]", gotParams.FindingIDs, gotParams.IgnoreFindingIDs)
	}
	if lookedUpActive.Load() {
		t.Fatal("--run must not resolve the active run of the current branch")
	}
	for _, want := range []string{"recorded:", "outcome: passed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestAxiRespond_RunFlagRefusesAnUnknownRunID(t *testing.T) {
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(context.Context, json.RawMessage) (interface{}, error) {
			responded.Store(true)
			return &ipc.RespondResult{OK: true}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		return nil, errors.New("run not found: run-gone")
	})
	chdir(t, t.TempDir())

	out, err := executeCmd("axi", "respond", "--run", "run-gone", "--action", "approve", "--wait", "3s")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("error = %v, want exit 1\n%s", err, out)
	}
	if !strings.Contains(out, "no run with id run-gone") {
		t.Fatalf("output missing the unknown-id refusal:\n%s", out)
	}
	if responded.Load() {
		t.Fatal("an unknown run id must not send a response")
	}
}

func TestAxiRespond_RunFlagRefusesARunThatIsNotAtAGate(t *testing.T) {
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(context.Context, json.RawMessage) (interface{}, error) {
			responded.Store(true)
			return &ipc.RespondResult{OK: true}, nil
		},
	})
	for name, run := range map[string]func() *ipc.RunInfo{
		"running":   fx.running,
		"completed": fx.completed,
	} {
		t.Run(name, func(t *testing.T) {
			fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) { return run(), nil })
			out, err := executeCmd("axi", "respond", "--run", "run-timeout", "--action", "approve", "--wait", "3s")
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != 1 {
				t.Fatalf("error = %v, want exit 1\n%s", err, out)
			}
			if !strings.Contains(out, "run run-timeout is not parked at a gate") {
				t.Fatalf("output missing the not-at-a-gate refusal:\n%s", out)
			}
			if responded.Load() {
				t.Fatal("a run that is not at a gate must not receive a response")
			}
		})
	}
}

func TestAxiRespond_WithoutRunFlagStillAnswersTheActiveRun(t *testing.T) {
	var gotParams ipc.RespondParams
	var responded, lookedUpActive atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			if err := json.Unmarshal(raw, &gotParams); err != nil {
				return nil, err
			}
			responded.Store(true)
			return &ipc.RespondResult{OK: true}, nil
		},
	})
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) {
		lookedUpActive.Store(true)
		return fx.awaiting(), nil
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		if responded.Load() {
			return fx.completed(), nil
		}
		return fx.awaiting(), nil
	})

	out, err := executeCmd("axi", "respond", "--action", "approve", "--wait", "3s")
	if err != nil {
		t.Fatalf("respond: %v\n%s", err, out)
	}
	if !lookedUpActive.Load() || gotParams.RunID != "run-timeout" {
		t.Fatalf("active lookup = %v, responded to %q; want the current branch's run-timeout", lookedUpActive.Load(), gotParams.RunID)
	}
}

func TestAxiRespond_RunFlagGateHelpCarriesTheRunID(t *testing.T) {
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(context.Context, json.RawMessage) (interface{}, error) {
			responded.Store(true)
			return &ipc.RespondResult{OK: true}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		if responded.Load() {
			next := fx.awaiting()
			next.Steps[0].RoundCount++
			return next, nil
		}
		return fx.awaiting(), nil
	})
	chdir(t, t.TempDir())

	out, err := executeCmd("axi", "respond", "--run", "run-timeout", "--action", "approve", "--wait", "3s")
	if err != nil {
		t.Fatalf("respond --run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"`no-mistakes axi respond --run run-timeout --action approve`",
		"`no-mistakes axi respond --run run-timeout --action skip`",
		"axi logs --run run-timeout --step",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("gate help missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "axi respond --action") {
		t.Fatalf("a --run call printed a follow-up respond command without the run id:\n%s", out)
	}
}

func TestAxiRespond_RunFlagRefusesAnEmptyValueInsteadOfAnsweringTheBranchRun(t *testing.T) {
	for _, value := range []string{"", "   "} {
		var responded, lookedUpActive atomic.Bool
		fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
			respond: func(context.Context, json.RawMessage) (interface{}, error) {
				responded.Store(true)
				return &ipc.RespondResult{OK: true}, nil
			},
		})
		fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) {
			lookedUpActive.Store(true)
			return fx.awaiting(), nil
		})

		out, err := executeCmd("axi", "respond", "--run", value, "--action", "approve", "--wait", "3s")
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Fatalf("--run %q: error = %v, want exit 2\n%s", value, err, out)
		}
		if !strings.Contains(out, "--run requires a run id") {
			t.Fatalf("--run %q: output missing the refusal:\n%s", value, out)
		}
		if responded.Load() || lookedUpActive.Load() {
			t.Fatalf("--run %q must neither answer nor resolve the branch's run", value)
		}
	}
}

func TestAxiRespond_RunFlagRefusalHelpCarriesTheRunID(t *testing.T) {
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(context.Context, json.RawMessage) (interface{}, error) {
			return &ipc.RespondResult{OK: false, Refusal: "refused", Help: "inspect it with `no-mistakes axi status`, then retry"}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) { return fx.awaiting(), nil })
	chdir(t, t.TempDir())

	out, err := executeCmd("axi", "respond", "--run", "run-timeout", "--action", "fix", "--findings", "R1", "--wait", "3s")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("error = %v, want exit 2\n%s", err, out)
	}
	if !strings.Contains(out, "`no-mistakes axi status --run run-timeout`") {
		t.Fatalf("refusal help dropped the run id:\n%s", out)
	}
}
