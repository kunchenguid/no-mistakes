package cli

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// respondFixture parks the run at gate, records what a respond sent, and
// completes the run once one arrives.
func respondFixture(t *testing.T, gate ipc.StepResultInfo, respond func() error) (*atomic.Bool, *ipc.RespondParams) {
	t.Helper()
	var got ipc.RespondParams
	responded := new(atomic.Bool)
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			if err := json.Unmarshal(raw, &got); err != nil {
				return nil, err
			}
			if respond != nil {
				if err := respond(); err != nil {
					return nil, err
				}
			}
			responded.Store(true)
			return &ipc.RespondResult{OK: true}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		if responded.Load() {
			return fx.completed(), nil
		}
		run := fx.running()
		run.Steps = []ipc.StepResultInfo{gate}
		return run, nil
	})
	return responded, &got
}

func parkedGate(step types.StepName, findings string) ipc.StepResultInfo {
	gate := ipc.StepResultInfo{StepName: step, Status: types.StepStatusAwaitingApproval}
	if findings != "" {
		gate.FindingsJSON = &findings
	}
	return gate
}

func wantUsageRefusal(t *testing.T, out string, err error, responded *atomic.Bool, want string) {
	t.Helper()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("error = %v, want usage exit 2\n%s", err, out)
	}
	if !strings.Contains(out, want) {
		t.Fatalf("output missing %q:\n%s", want, out)
	}
	if responded.Load() {
		t.Fatal("a refused response still reached the daemon")
	}
}

// A skip is recorded with the operator's reason, so the reason is not
// optional, and a missing one is refused before anything reaches the daemon.
func TestAxiRespond_SkipRequiresAReason(t *testing.T) {
	for name, args := range map[string][]string{
		"absent": {"axi", "respond", "--action", "skip", "--wait", "3s"},
		"blank":  {"axi", "respond", "--action", "skip", "--reason", "  ", "--wait", "3s"},
	} {
		t.Run(name, func(t *testing.T) {
			responded, _ := respondFixture(t, parkedGate(types.StepReview, ""), nil)
			out, err := executeCmd(args...)
			wantUsageRefusal(t, out, err, responded, "--action skip requires --reason")
		})
	}
}

func TestAxiRespond_SkipForwardsItsReason(t *testing.T) {
	responded, got := respondFixture(t, parkedGate(types.StepReview, ""), nil)
	out, err := executeCmd("axi", "respond", "--action", "skip", "--reason", "  reviewed upstream  ", "--wait", "3s")
	if err != nil {
		t.Fatalf("skip with a reason = %v\n%s", err, out)
	}
	if !responded.Load() || got.Action != types.ActionSkip || got.Step != types.StepReview || got.ApprovalReason != "reviewed upstream" {
		t.Fatalf("forwarded %+v, want the skip with the trimmed reason", got)
	}
}

// A CLI upgraded under a still-running older daemon must say so: that daemon
// refuses a skip's reason with a message about Test approval, before
// recording anything.
func TestAxiRespond_SkipExplainsAnOlderDaemonThatCannotRecordTheReason(t *testing.T) {
	for _, refusal := range []string{"an approval reason applies only to Test approval", "a reason applies only to Test approval or an abort"} {
		t.Run(refusal, func(t *testing.T) {
			respondFixture(t, parkedGate(types.StepReview, ""), func() error { return errors.New(refusal) })
			out, err := executeCmd("axi", "respond", "--action", "skip", "--reason", "reviewed upstream", "--wait", "3s")
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != 1 {
				t.Fatalf("error = %v, want exit 1\n%s", err, out)
			}
			for _, want := range []string{"too old to record a skip reason", "nothing was recorded", "no-mistakes daemon restart --force"} {
				if !strings.Contains(out, want) {
					t.Fatalf("output missing %q:\n%s", want, out)
				}
			}
		})
	}
}

// Approving a Test gate is refused without a reason exactly when the approval
// records a Test exception; approving a no-surface park completes normally
// and still needs none.
func TestAxiRespond_TestExceptionApprovalRequiresAReason(t *testing.T) {
	exceptions := map[string]string{
		"no-go":        `{"findings":[{"id":"t1","severity":"error","description":"refund fails"}],"verdict":"no-go"}`,
		"inconclusive": `{"findings":[],"verdict":"inconclusive"}`,
		"budget cut":   `{"findings":[{"id":"` + types.FindingIDTestAgentTimeout + `","severity":"warning","description":"cut"}]}`,
		"command":      `{"findings":[{"id":"cmd","severity":"error","category":"` + types.FindingCategoryTestCommand + `","description":"go test failed"}]}`,
		"unreadable":   `not json`,
	}
	for name, findings := range exceptions {
		t.Run(name, func(t *testing.T) {
			responded, _ := respondFixture(t, parkedGate(types.StepTest, findings), nil)
			out, err := executeCmd("axi", "respond", "--action", "approve", "--wait", "3s")
			wantUsageRefusal(t, out, err, responded, "records a Test exception, which requires --reason")
		})
	}

	t.Run("with a reason", func(t *testing.T) {
		responded, got := respondFixture(t, parkedGate(types.StepTest, exceptions["no-go"]), nil)
		out, err := executeCmd("axi", "respond", "--action", "approve", "--reason", "provider sandbox is down", "--wait", "3s")
		if err != nil || !responded.Load() || got.Action != types.ActionApprove || got.ApprovalReason != "provider sandbox is down" {
			t.Fatalf("approve with a reason = %v, forwarded %+v\n%s", err, got, out)
		}
	})

	t.Run("no-surface", func(t *testing.T) {
		responded, got := respondFixture(t, parkedGate(types.StepTest, `{"findings":[],"verdict":"no-surface"}`), nil)
		out, err := executeCmd("axi", "respond", "--action", "approve", "--wait", "3s")
		if err != nil || !responded.Load() || got.Action != types.ActionApprove || got.ApprovalReason != "" {
			t.Fatalf("no-surface approve = %v, forwarded %+v\n%s", err, got, out)
		}
	})
}

// The gate help names the reason wherever the response requires one.
func TestGateHelpNamesTheRequiredReason(t *testing.T) {
	noGo := stepView{Name: "test", Status: "awaiting_approval", FindingsJSON: `{"findings":[],"verdict":"no-go"}`}
	out := axiDoc(gateFields(noGo)...)
	for _, want := range []string{"--action approve --reason", "--action skip --reason"} {
		if !strings.Contains(out, want) {
			t.Fatalf("Test exception gate help missing %q:\n%s", want, out)
		}
	}
	review := axiDoc(gateFields(stepView{Name: "review", Status: "awaiting_approval", FindingsJSON: `{"findings":[]}`})...)
	if strings.Contains(review, "--action approve --reason") || !strings.Contains(review, "--action skip --reason") {
		t.Fatalf("review gate help asks for the wrong reasons:\n%s", review)
	}
}

// `axi run --skip` records the operator's reason on every skipped step, so it
// is refused without one, and the reason is refused without a skip.
func TestAxiRun_SkipRequiresASkipReason(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"absent":      {[]string{"axi", "run", "--skip", "lint", "--intent", "goal"}, "--skip requires --skip-reason"},
		"blank":       {[]string{"axi", "run", "--skip", "lint", "--skip-reason", " ", "--intent", "goal"}, "--skip requires --skip-reason"},
		"reason only": {[]string{"axi", "run", "--skip-reason", "CI lints", "--intent", "goal"}, "--skip-reason applies only with --skip"},
	} {
		t.Run(name, func(t *testing.T) {
			responded, _ := respondFixture(t, parkedGate(types.StepReview, ""), nil)
			out, err := executeCmd(tc.args...)
			wantUsageRefusal(t, out, err, responded, tc.want)
		})
	}
}

// The reason rides the push that starts the run and the rerun fallback.
func TestSkipReasonReachesTheDaemon(t *testing.T) {
	reason := "CI lints, commas, colons: ok\nand a second line"
	options := formatSkipPushOptions([]types.StepName{types.StepLint}, reason)
	got, err := parseOpaquePushOptions(append(options, "no-mistakes.intent=x"), skipReasonPushOptionPrefix, "skip reason")
	if err != nil || got != reason {
		t.Fatalf("push option round trip = %q, %v; want %q", got, err, reason)
	}
	if steps, err := parseSkipPushOptions(options); err != nil || !reflect.DeepEqual(steps, []types.StepName{types.StepLint}) {
		t.Fatalf("skip steps = %v, %v", steps, err)
	}
	if params := rerunParams("repo-1", "feature/x", []types.StepName{types.StepLint}, reason, "goal", ""); params.SkipReason != reason {
		t.Fatalf("rerun SkipReason = %q, want %q", params.SkipReason, reason)
	}
}
