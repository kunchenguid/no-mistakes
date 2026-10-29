package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/skill"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This checks the emitted help and generated skill as public text contracts.
func TestAxiRespond_RestoreGuidanceIncludesWindowsException(t *testing.T) {
	help, err := executeCmd("axi", "respond", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string]string{"CLI help": help, "generated skill": skill.Markdown()} {
		output = strings.Join(strings.Fields(strings.ReplaceAll(output, "`", "")), " ")
		for _, want := range []string{
			"git -c core.symlinks=true -c core.autocrlf=<saved> -c core.eol=<saved> -c apply.whitespace=nowarn stash apply --index <snapshot>",
			"On Windows without symlink creation support, omit -c core.symlinks=true from both commands.",
			"That Windows procedure does not support symlink restoration.",
		} {
			if !strings.Contains(output, want) {
				t.Errorf("%s missing restore guidance %q", name, want)
			}
		}
	}
}

// `axi respond --action abort` hands the executor's existing gate abort the
// parked gate's identity and the operator's reason, and reports the rejection
// ahead of the terminal failed outcome it leads to.
func TestAxiRespond_AbortForwardsTheGateAbortWithItsReason(t *testing.T) {
	var got ipc.RespondParams
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			if err := json.Unmarshal(raw, &got); err != nil {
				return nil, err
			}
			responded.Store(true)
			return &ipc.RespondResult{OK: true}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		if !responded.Load() {
			return fx.awaiting(), nil
		}
		run := fx.running()
		run.Status = types.RunFailed
		msg := "step review: " + types.UserAbortStepError("unsafe to land")
		run.Error = &msg
		return run, nil
	})

	out, err := executeCmd("axi", "respond", "--action", "abort", "--reason", "  unsafe to land  ", "--wait", "3s")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("a rejected gate ends the run failed, want exit 1; got %v\n%s", err, out)
	}
	if got.Action != types.ActionAbort || got.Step != types.StepReview || got.RunID != "run-timeout" {
		t.Fatalf("forwarded %+v, want abort of run-timeout at the review gate", got)
	}
	if got.ApprovalReason != "unsafe to land" {
		t.Fatalf("forwarded reason = %q, want the trimmed operator reason", got.ApprovalReason)
	}
	if len(got.FindingIDs) != 0 || len(got.IgnoreFindingIDs) != 0 || len(got.AddedFindings) != 0 {
		t.Fatalf("an abort must not select, ignore or add findings: %+v", got)
	}
	for _, want := range []string{"rejected:", "run-timeout", "review", "unsafe to land", "outcome: failed", "aborted by user: unsafe to land"} {
		if !strings.Contains(out, want) {
			t.Fatalf("abort output missing %q:\n%s", want, out)
		}
	}
}

// An abort records a rejection, so the reason is not optional, and a missing
// one is refused before anything reaches the daemon.
func TestAxiRespond_AbortRequiresAReason(t *testing.T) {
	for name, args := range map[string][]string{
		"absent": {"axi", "respond", "--action", "abort", "--wait", "3s"},
		"blank":  {"axi", "respond", "--action", "abort", "--reason", "   ", "--wait", "3s"},
	} {
		t.Run(name, func(t *testing.T) {
			var responded atomic.Bool
			fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
				respond: func(context.Context, json.RawMessage) (interface{}, error) {
					responded.Store(true)
					return &ipc.RespondResult{OK: true}, nil
				},
			})
			fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) { return fx.awaiting(), nil })

			out, err := executeCmd(args...)
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != 2 {
				t.Fatalf("error = %v, want usage exit 2\n%s", err, out)
			}
			if !strings.Contains(out, "--action abort requires --reason") {
				t.Fatalf("usage output missing the reason contract:\n%s", out)
			}
			if responded.Load() {
				t.Fatal("an abort without a reason still reached the daemon")
			}
		})
	}
}

// The abort contract is the gate's: a run that is not parked there, or an
// unknown run, is refused without a response being sent.
func TestAxiRespond_AbortRefusesARunThatIsNotAtAGate(t *testing.T) {
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(context.Context, json.RawMessage) (interface{}, error) {
			responded.Store(true)
			return &ipc.RespondResult{OK: true}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) { return fx.running(), nil })

	out, err := executeCmd("axi", "respond", "--run", "run-timeout", "--action", "abort", "--reason", "no", "--wait", "3s")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("error = %v, want exit 1\n%s", err, out)
	}
	if !strings.Contains(out, "run run-timeout is not parked at a gate") {
		t.Fatalf("output missing the not-at-a-gate refusal:\n%s", out)
	}
	if responded.Load() {
		t.Fatal("a run that is not at a gate received an abort")
	}
}

// --reason keeps its old contract for the other actions: only a Test approval
// exception carries one.
func TestAxiRespond_ReasonStillRefusedOutsideTestApprovalAndAbort(t *testing.T) {
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(context.Context, json.RawMessage) (interface{}, error) {
			responded.Store(true)
			return &ipc.RespondResult{OK: true}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) { return fx.awaiting(), nil })

	for _, action := range []string{"approve", "skip"} {
		out, err := executeCmd("axi", "respond", "--action", action, "--reason", "why", "--wait", "3s")
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Fatalf("%s: error = %v, want usage exit 2\n%s", action, err, out)
		}
		if !strings.Contains(out, "--reason applies only to --action approve on the Test step, or to --action abort") {
			t.Fatalf("%s: usage output missing the reason contract:\n%s", action, out)
		}
	}
	if responded.Load() {
		t.Fatal("a refused --reason still reached the daemon")
	}
}

func TestAxiRespond_UnknownActionListsAbort(t *testing.T) {
	newAxiTimeoutFixture(t, axiTimeoutOpts{})
	out, err := executeCmd("axi", "respond", "--action", "reject", "--wait", "3s")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("error = %v, want usage exit 2\n%s", err, out)
	}
	if !strings.Contains(out, "Valid actions: approve, fix, skip, abort") {
		t.Fatalf("usage output missing the action list:\n%s", out)
	}
}

// A CLI upgraded under a still-running older daemon must say so: that daemon
// refuses the abort's reason with a message about Test approval, which would
// otherwise read as the operator's mistake. It refused before recording
// anything, and the restart needs --force while the gate's run is active.
func TestAxiRespond_AbortExplainsAnOlderDaemonThatCannotRecordTheReason(t *testing.T) {
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(context.Context, json.RawMessage) (interface{}, error) {
			return nil, errors.New("an approval reason applies only to Test approval")
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) { return fx.awaiting(), nil })

	out, err := executeCmd("axi", "respond", "--action", "abort", "--reason", "unsafe to land", "--wait", "3s")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("error = %v, want exit 1\n%s", err, out)
	}
	for _, want := range []string{"too old to record an abort reason", "nothing was recorded", "no-mistakes daemon restart --force"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "applies only to Test approval") {
		t.Fatalf("the misleading legacy refusal leaked into the output:\n%s", out)
	}
	// `daemon status` never lists runs, so it must not be offered as the way
	// to review what a forced restart interrupts.
	if strings.Contains(out, "daemon status") {
		t.Fatalf("hint points at `daemon status`, which does not list runs:\n%s", out)
	}
	if !strings.Contains(out, "lists every run") {
		t.Fatalf("hint does not say where the interrupted runs are listed:\n%s", out)
	}
}
