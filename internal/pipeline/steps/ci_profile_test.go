package steps

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type exhaustedProfileInvoker struct {
	tiers []int
}

func (i *exhaustedProfileInvoker) Invoke(_ context.Context, req agent.InvocationRequest) (*agent.Result, error) {
	i.tiers = append(i.tiers, req.Tier)
	return nil, &agent.ProfileUnavailableError{Profile: "fix_balanced", Cause: context.DeadlineExceeded}
}

func TestCIStep_ProfileExhaustionStopsWithoutTierJump(t *testing.T) {
	upstream, dir, baseSHA, headSHA := setupCIRepublish(t)
	checksJSON := `[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`
	invoker := &exhaustedProfileInvoker{}
	sctx := newTestContextWithDBRecords(t, nil, dir, baseSHA, headSHA, config.Commands{})
	sctx.Invoker = invoker
	sctx.Env = fakeCIGH(t, "OPEN", checksJSON)
	sctx.Repo.UpstreamURL = "https://github.com/test/repo.git"
	sctx.Repo.ForkURL = upstream
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = time.Minute
	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	round, err := sctx.DB.ReserveStepRound(stepResult.ID, 1, "initial")
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID
	sctx.CurrentRound = round

	waits := 0
	step := &CIStep{
		checksGracePeriod: time.Nanosecond,
		waitForNextPoll: func(context.Context, time.Duration) error {
			waits++
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, want unresolved CI failure requiring approval", outcome)
	}
	if len(invoker.tiers) != 1 || invoker.tiers[0] != 0 {
		t.Fatalf("invoked tiers = %v, want only exhausted profile tier 0", invoker.tiers)
	}
	if waits != 0 {
		t.Fatalf("poll waits = %d, want immediate terminal outcome after profile exhaustion", waits)
	}
	repairs, err := sctx.DB.GetFindingRepairsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repairs) != 0 {
		t.Fatalf("durable repairs = %+v, want no spent quality tier after profile exhaustion", repairs)
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != headSHA {
		t.Fatalf("remote changed to %s after profile exhaustion; want %s", got, headSHA)
	}
}
