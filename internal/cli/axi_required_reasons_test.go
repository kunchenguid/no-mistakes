package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// reasonsDaemon is one fake daemon shared by every respond case below, so the
// cases pay for one repository and one server rather than one each. A case
// parks the run at its gate and optionally makes the respond call fail; once a
// response is accepted, the run reads back as completed.
type reasonsDaemon struct {
	mu        sync.Mutex
	gate      ipc.StepResultInfo
	refuse    error
	responded bool
	got       ipc.RespondParams
}

func (d *reasonsDaemon) park(gate ipc.StepResultInfo, refuse error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gate, d.refuse, d.responded, d.got = gate, refuse, false, ipc.RespondParams{}
}

func (d *reasonsDaemon) response() (bool, ipc.RespondParams) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.responded, d.got
}

func newReasonsDaemon(t *testing.T) *reasonsDaemon {
	t.Helper()
	d := &reasonsDaemon{}
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			if err := json.Unmarshal(raw, &d.got); err != nil {
				return nil, err
			}
			if d.refuse != nil {
				return nil, d.refuse
			}
			d.responded = true
			return &ipc.RespondResult{OK: true}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.responded {
			return fx.completed(), nil
		}
		run := fx.running()
		run.Steps = []ipc.StepResultInfo{d.gate}
		return run, nil
	})
	return d
}

func parkedGate(step types.StepName, findings string) ipc.StepResultInfo {
	gate := ipc.StepResultInfo{StepName: step, Status: types.StepStatusAwaitingApproval}
	if findings != "" {
		gate.FindingsJSON = &findings
	}
	return gate
}

func wantUsageRefusal(t *testing.T, out string, err error, want string) {
	t.Helper()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("error = %v, want usage exit 2\n%s", err, out)
	}
	if !strings.Contains(out, want) {
		t.Fatalf("output missing %q:\n%s", want, out)
	}
}

// `axi respond` requires the operator's reason for a skip and for a Test
// approval that records a Test exception; a refused response never reaches
// the daemon, and an accepted one forwards the trimmed reason.
func TestAxiRespond_RequiredReasons(t *testing.T) {
	d := newReasonsDaemon(t)
	review := parkedGate(types.StepReview, "")
	noGo := `{"findings":[{"id":"t1","severity":"error","description":"refund fails"}],"verdict":"no-go"}`

	refusals := []struct {
		name string
		gate ipc.StepResultInfo
		args []string
		want string
	}{
		{"skip without a reason", review, []string{"--action", "skip"}, "--action skip requires --reason"},
		{"skip with a blank reason", review, []string{"--action", "skip", "--reason", "  "}, "--action skip requires --reason"},
		{"reason on a review approval", review, []string{"--action", "approve", "--reason", "why"}, "--reason applies only to --action approve on the Test step, --action skip, or --action abort"},
		{"no-go approval", parkedGate(types.StepTest, noGo), []string{"--action", "approve"}, "records a Test exception, which requires --reason"},
		{"inconclusive approval", parkedGate(types.StepTest, `{"findings":[],"verdict":"inconclusive"}`), []string{"--action", "approve"}, "records a Test exception, which requires --reason"},
		{"budget-cut approval", parkedGate(types.StepTest, `{"findings":[{"id":"`+types.FindingIDTestAgentTimeout+`","severity":"warning","description":"cut"}]}`), []string{"--action", "approve"}, "records a Test exception, which requires --reason"},
		{"failing-command approval", parkedGate(types.StepTest, `{"findings":[{"id":"cmd","severity":"error","category":"`+types.FindingCategoryTestCommand+`","description":"go test failed"}]}`), []string{"--action", "approve"}, "records a Test exception, which requires --reason"},
		{"unreadable-findings approval", parkedGate(types.StepTest, `not json`), []string{"--action", "approve", "--reason", " "}, "records a Test exception, which requires --reason"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			d.park(tc.gate, nil)
			out, err := executeCmd(append(append([]string{"axi", "respond"}, tc.args...), "--wait", "3s")...)
			wantUsageRefusal(t, out, err, tc.want)
			if responded, _ := d.response(); responded {
				t.Fatal("a refused response still reached the daemon")
			}
		})
	}

	accepted := []struct {
		name       string
		gate       ipc.StepResultInfo
		args       []string
		wantAction types.ApprovalAction
		wantReason string
	}{
		{"skip with a reason", review, []string{"--action", "skip", "--reason", "  reviewed upstream  "}, types.ActionSkip, "reviewed upstream"},
		{"exception approval with a reason", parkedGate(types.StepTest, noGo), []string{"--action", "approve", "--reason", "provider sandbox is down"}, types.ActionApprove, "provider sandbox is down"},
		{"no-surface approval needs none", parkedGate(types.StepTest, `{"findings":[],"verdict":"no-surface"}`), []string{"--action", "approve"}, types.ActionApprove, ""},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			d.park(tc.gate, nil)
			out, err := executeCmd(append(append([]string{"axi", "respond"}, tc.args...), "--wait", "3s")...)
			responded, got := d.response()
			if err != nil || !responded || got.Action != tc.wantAction || got.Step != tc.gate.StepName || got.ApprovalReason != tc.wantReason {
				t.Fatalf("respond = %v, forwarded %+v (responded %v)\n%s", err, got, responded, out)
			}
		})
	}

	// A CLI upgraded under a still-running older daemon must say so: that
	// daemon refuses a skip's reason with a message about Test approval,
	// before recording anything.
	for _, refusal := range []string{"an approval reason applies only to Test approval", "a reason applies only to Test approval or an abort"} {
		t.Run("older daemon: "+refusal, func(t *testing.T) {
			d.park(review, errors.New(refusal))
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
// is refused without one, and the reason is refused without a skip. Both are
// flag refusals, decided before any daemon is contacted.
func TestAxiRun_SkipRequiresASkipReason(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"absent":      {[]string{"--skip", "lint", "--intent", "goal"}, "--skip requires --skip-reason"},
		"blank":       {[]string{"--skip", "lint", "--skip-reason", " ", "--intent", "goal"}, "--skip requires --skip-reason"},
		"reason only": {[]string{"--skip-reason", "CI lints", "--intent", "goal"}, "--skip-reason applies only with --skip"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := newAxiRunCmd()
			cmd.SetArgs(tc.args)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			err := cmd.Execute()
			wantUsageRefusal(t, out.String(), err, tc.want)
		})
	}
}

// The reason rides the push that starts the run and the rerun fallback.
func TestSkipReasonReachesTheDaemon(t *testing.T) {
	reason := "CI lints, commas, colons: ok\nand a second line"
	options := formatSkipPushOptions([]types.StepName{types.StepLint}, reason)
	wantWireReason := "no-mistakes.skip-reason=" + base64.StdEncoding.EncodeToString([]byte(reason))
	if len(options) != 2 || options[1] != wantWireReason {
		t.Fatalf("skip reason push option = %v; want %q", options, wantWireReason)
	}
	if steps, err := parseSkipPushOptions(options); err != nil || !reflect.DeepEqual(steps, []types.StepName{types.StepLint}) {
		t.Fatalf("skip steps = %v, %v", steps, err)
	}
	if params := rerunParams("repo-1", "feature/x", []types.StepName{types.StepLint}, reason, "goal", ""); params.SkipReason != reason {
		t.Fatalf("rerun SkipReason = %q, want %q", params.SkipReason, reason)
	}
}

// An older daemon would drop the unknown skip_reason and skip the steps
// without it, so a launch carrying one probes first and refuses a daemon
// without the capability before any launch request reaches it. The current
// daemon's answer is exercised by the e2e journey TestRequiredSkipReasonJourney.
func TestAxiRunSkipReasonRefusesAnOlderDaemon(t *testing.T) {
	launched := olderDaemonFixture(t, nil)
	writeGlobalConfig(t, "intent:\n  publish_intent: true\n")
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	err := runAxiRunWithPRText(cmd, false, []types.StepName{types.StepLint}, "CI lints", "goal", "", false, "", "", defaultAxiWait, db.RunPRText{})
	if err == nil || !strings.Contains(out.String(), "too old to record a skip reason") {
		t.Fatalf("axi run = %v, want the older daemon refused:\n%s", err, out.String())
	}
	if len(*launched) != 0 {
		t.Fatalf("a skip reason the daemon would drop reached it: %v", *launched)
	}
}
