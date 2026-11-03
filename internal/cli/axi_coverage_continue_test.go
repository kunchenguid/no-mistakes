package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestAxiCoverageContinuationRefusesFixOverridesAndAutomaticApprovals(t *testing.T) {
	for _, flags := range [][]string{
		{"--yes"},
		{"--findings", "r1"},
		{"--ignore", "r1"},
		{"--instructions", "change code"},
		{"--add-finding", `{"description":"fake"}`},
		{"--reason", "waiver"},
	} {
		t.Run(flags[0], func(t *testing.T) {
			t.Setenv("NM_HOME", t.TempDir())
			chdir(t, t.TempDir())
			args := append([]string{"axi", "respond", "--action", "complete-coverage"}, flags...)
			out, err := executeCmd(args...)
			if err == nil || !strings.Contains(out, "cannot approve subsequent gates or change findings") {
				t.Fatalf("coverage continuation did not refuse unsafe flags before daemon dispatch: err=%v output=%s", err, out)
			}
		})
	}
}

func TestAxiCoverageContinuationBindsTheObservedHeadStepAndRound(t *testing.T) {
	testCoverageContinuationCLI(t, executeCmd)
}

func TestNoMistakesBinary_CoverageContinuation(t *testing.T) {
	bin := buildNoMistakesBinary(t)
	testCoverageContinuationCLI(t, func(args ...string) (string, error) {
		return execNoMistakesErr(t, bin, args...)
	})
}

func testCoverageContinuationCLI(t *testing.T, invoke func(...string) (string, error)) {
	t.Helper()
	var got map[string]any
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		if err := json.Unmarshal(raw, &got); err != nil {
			return nil, err
		}
		responded.Store(true)
		return &ipc.RespondResult{OK: true}, nil
	}})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		if responded.Load() {
			return fx.completed(), nil
		}
		run := fx.awaiting()
		run.HeadSHA = "current-head"
		if err := json.Unmarshal([]byte(`{"id":"review-step","step_name":"review","latest_round_id":"parked-round","findings_json":"{\"findings\":[]}"}`), &run.Steps[0]); err != nil {
			return nil, err
		}
		return run, nil
	})
	out, err := invoke("axi", "respond", "--step", "review", "--action", "complete-coverage", "--wait", "3s")
	if err != nil {
		t.Fatalf("coverage response failed: %v\n%s", err, out)
	}
	binding, ok := got["coverage"].(map[string]any)
	if got["action"] != "complete-coverage" || !ok || binding["head_sha"] != "current-head" || binding["step_result_id"] != "review-step" || binding["round_id"] != "parked-round" {
		t.Fatalf("coverage response lost its observed binding: %+v", got)
	}
	if got["finding_ids"] != nil || got["added_findings"] != nil || got["ignore_finding_ids"] != nil {
		t.Fatal("coverage response manufactured a finding decision")
	}
}
