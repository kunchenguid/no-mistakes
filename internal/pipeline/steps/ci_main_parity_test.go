package steps

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIStep_NoHandoffReadinessClearFailsClosed(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "issues", true: "protected-path retry"}[retry], func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			url := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &url
			sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"build","state":"FAILURE","bucket":"fail"}]`)
			writeErr := errors.New("readiness write failed")
			writes := 0
			sctx.SetCIReadiness = func(ready, declared bool) error {
				writes++
				if ready || declared {
					t.Fatal("clear attempted to set readiness")
				}
				return writeErr
			}
			if retry {
				sctx.Fixing = true
				sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
				if err != nil {
					t.Fatal(err)
				}
				sctx.StepResultID = sr.ID
				sctx.PreviousFindings = `{"findings":[{"id":"protected-path-refusal","severity":"error","description":"refused"}]}`
				if err := sctx.DB.SetStepFindings(sctx.StepResultID, sctx.PreviousFindings); err != nil {
					t.Fatal(err)
				}
			}
			outcome, err := (&CIStep{}).Execute(sctx)
			if retry {
				// The retained-repair wrapper keeps the failure parked, as on main.
				if err != nil || outcome == nil || !outcome.NeedsApproval || !strings.Contains(outcome.Findings, writeErr.Error()) {
					t.Fatalf("retry = (%+v, %v), want parked write failure", outcome, err)
				}
			} else if !errors.Is(err, writeErr) || outcome != nil {
				t.Fatalf("issues = (%+v, %v), want write error", outcome, err)
			}
			if writes != 1 {
				t.Fatalf("writes = %d, want 1", writes)
			}
		})
	}
}

func TestCIStep_TimeoutClearsReadinessOnlyForPendingHandoff(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "no handoff", true: "pending handoff"}[pending], func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			url := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &url
			sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)
			sctx.Config.CITimeout = time.Minute
			sctx.HandoffPending = func() bool { return pending }
			clock := time.Now()
			step := &CIStep{now: func() time.Time { return clock }, baseBranchTip: func(context.Context) (string, bool) { return base, true }}
			step.waitForNextPoll = func(context.Context, time.Duration) error { clock = clock.Add(time.Minute); return nil }
			outcome, err := step.Execute(sctx)
			if err != nil || outcome == nil {
				t.Fatalf("timeout = (%+v, %v)", outcome, err)
			}
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if (run.CIReadyAt == nil) != pending {
				t.Fatalf("readiness = %v, pending = %t", run.CIReadyAt, pending)
			}
		})
	}
}
