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

// A refused fix response leaves the gate parked, so the caller gets the
// daemon's own wording plus the unaccounted IDs and the next action, not a
// generic transport failure.
func TestAxiRespond_RendersARefusalWithTheUnaccountedIDs(t *testing.T) {
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(context.Context, json.RawMessage) (interface{}, error) {
			return &ipc.RespondResult{
				OK:      false,
				Refusal: "R2,R3 not addressed: list them in --findings or --ignore",
				Missing: []string{"R2", "R3"},
				Help:    "List every finding the gate shows in --findings or --ignore.",
			}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		return fx.awaiting(), nil
	})

	out, err := executeCmd("axi", "respond", "--action", "fix", "--findings", "R1", "--wait", "3s")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("refusal error = %v, want usage exit 2\n%s", err, out)
	}
	for _, want := range []string{
		"R2,R3 not addressed: list them in --findings or --ignore",
		"Unaccounted finding IDs: R2,R3",
		"List every finding the gate shows in --findings or --ignore.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("refusal output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "outcome:") || strings.Contains(out, "gate:") {
		t.Fatalf("a refusal must not be reported as a resolved gate:\n%s", out)
	}
}

// The successful response echoes what it recorded and forwards the explicit
// declines, including the findings it kept by omission.
func TestAxiRespond_EchoesRecordedDispositionsAndForwardsIgnores(t *testing.T) {
	var gotParams ipc.RespondParams
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			if err := json.Unmarshal(raw, &gotParams); err != nil {
				return nil, err
			}
			responded.Store(true)
			return &ipc.RespondResult{
				OK:      true,
				Fixed:   []string{"R1"},
				Ignored: []string{"R2"},
				Kept:    []string{"R3"},
			}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		if responded.Load() {
			return fx.completed(), nil
		}
		return fx.awaiting(), nil
	})

	out, err := executeCmd("axi", "respond", "--action", "fix", "--findings", "R1", "--ignore", "R2", "--wait", "3s")
	if err != nil {
		t.Fatalf("respond: %v\n%s", err, out)
	}
	t.Logf("respond output:\n%s", out)
	if strings.Join(gotParams.FindingIDs, ",") != "R1" {
		t.Fatalf("forwarded findings = %v, want [R1]", gotParams.FindingIDs)
	}
	if strings.Join(gotParams.IgnoreFindingIDs, ",") != "R2" {
		t.Fatalf("forwarded ignores = %v, want [R2]", gotParams.IgnoreFindingIDs)
	}
	for _, want := range []string{"recorded:", "fixed", "R1", "ignored", "R2", "kept", "R3", "outcome: passed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("respond output missing %q:\n%s", want, out)
		}
	}
}

// A --ignore naming a finding an earlier round of the same step already chose
// to fix is refused too: the CLI shows the daemon's out-of-scope help instead
// of a generic failure, and names the findings so the caller can correct the
// response.
func TestAxiRespond_RendersTheRefusalOfAnAlreadyFixedDecline(t *testing.T) {
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(context.Context, json.RawMessage) (interface{}, error) {
			return &ipc.RespondResult{
				OK:                 false,
				Refusal:            "R1 already chosen to fix in an earlier round of this step",
				DeclinedEarlierFix: []string{"R1"},
				Help:               "Reverting an applied fix is out of scope for a gate response: leave those findings out of --ignore to keep the earlier decision, or list them in --findings to have the pipeline fix them again.",
			}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		return fx.awaiting(), nil
	})

	out, err := executeCmd("axi", "respond", "--action", "fix", "--findings", "R2", "--ignore", "R1", "--wait", "3s")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("refusal error = %v, want usage exit 2\n%s", err, out)
	}
	for _, want := range []string{
		"R1 already chosen to fix in an earlier round of this step",
		"out of scope for a gate response",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("refusal output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Unaccounted finding IDs") {
		t.Fatalf("an already-fixed finding is accounted for, not missing:\n%s", out)
	}
}

// --ignore is part of the fix contract, never a silent no-op on another action.
func TestAxiRespond_IgnoreIsRejectedForOtherActions(t *testing.T) {
	responded := atomic.Bool{}
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(context.Context, json.RawMessage) (interface{}, error) {
			responded.Store(true)
			return &ipc.RespondResult{OK: true}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		return fx.awaiting(), nil
	})

	out, err := executeCmd("axi", "respond", "--action", "approve", "--ignore", "R1", "--wait", "3s")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("error = %v, want usage exit 2\n%s", err, out)
	}
	if !strings.Contains(out, "--findings and --ignore apply only to --action fix") {
		t.Fatalf("usage output missing the flag contract:\n%s", out)
	}
	if responded.Load() {
		t.Fatal("an invalid --ignore still reached the daemon")
	}
}

// An ignore-only fix response is valid: the operator may be keeping earlier
// decisions and declining only what this gate added, which is not the same as
// approving the gate. The CLI must forward it rather than failing its own
// precondition before the daemon ever sees it.
func TestAxiRespond_ForwardsAnIgnoreOnlyFixResponse(t *testing.T) {
	var gotParams ipc.RespondParams
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			if err := json.Unmarshal(raw, &gotParams); err != nil {
				return nil, err
			}
			responded.Store(true)
			return &ipc.RespondResult{OK: true, Ignored: []string{"R2"}}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		if responded.Load() {
			return fx.completed(), nil
		}
		return fx.awaiting(), nil
	})

	out, err := executeCmd("axi", "respond", "--action", "fix", "--ignore", "R2", "--wait", "3s")
	if err != nil {
		t.Fatalf("an ignore-only fix response must be accepted: %v\n%s", err, out)
	}
	if len(gotParams.FindingIDs) != 0 {
		t.Fatalf("forwarded findings = %v, want none", gotParams.FindingIDs)
	}
	if strings.Join(gotParams.IgnoreFindingIDs, ",") != "R2" {
		t.Fatalf("forwarded ignores = %v, want [R2]", gotParams.IgnoreFindingIDs)
	}
	for _, want := range []string{"recorded:", "ignored", "R2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("respond output missing %q:\n%s", want, out)
		}
	}
}
