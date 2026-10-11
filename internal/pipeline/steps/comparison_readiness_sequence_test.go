package steps

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIStepLiveRetargetWithNoClaimsWithholdsReadiness(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	c := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	gitCmd(t, dir, "branch", "dev", base)
	c.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	c.Run.PRURL = &prURL
	if err := c.DB.UpdateRunPRURL(c.Run.ID, prURL); err != nil {
		t.Fatal(err)
	}
	c.Config.CITimeout = -1
	var changed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/2.0/repositories/test/repo/pullrequests/42":
			target := "main"
			if changed.Load() {
				target = "dev"
			}
			fmt.Fprintf(w, `{"id":42,"state":"OPEN","links":{"html":{"href":%q}},"source":{"branch":{"name":"feature"},"repository":{"full_name":"test/repo"},"commit":{"hash":%q}},"destination":{"branch":{"name":%q}}}`, prURL, head, target)
		case "/2.0/repositories/test/repo/pullrequests/42/statuses":
			status := "INPROGRESS"
			if changed.Load() {
				status = "SUCCESSFUL"
			}
			fmt.Fprintf(w, `{"values":[{"key":"test","name":"test","state":%q,"links":{"commit":{"href":"https://api.bitbucket.org/2.0/repositories/test/repo/commit/%s"}}}]}`, status, head)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected request", 500)
		}
	}))
	defer server.Close()
	for _, env := range fakeBitbucketEnv(server.URL) {
		k, v, _ := strings.Cut(env, "=")
		t.Setenv(k, v)
	}
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Ctx = ctx
	polls := 0
	gotReady, readyEvent, guardRejected := false, false, false
	ci := (&CIStep{}).SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
		polls++
		if polls == 1 {
			changed.Store(true)
			return nil
		}
		run, err := c.DB.GetRun(c.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		gotReady = run.CIReadyAt != nil
		receipt, err := c.DB.GetRunPRContext(c.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		c.PRContext = receipt
		_, err = GuardPRContext(c, types.StepCI)
		guardRejected = err != nil
		t.Logf("before Execute returns: ready=%t event=%t pinnedTarget=%s liveTarget=dev liveHead=%s boundaryRecheck=%v", gotReady, readyEvent, receipt.TargetBranch, head, err)
		cancel()
		return ctx.Err()
	})
	review := contractSequenceStep{types.StepReview, func(c *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		return &pipeline.StepOutcome{Findings: `{"findings":[],"summary":"clean"}`, ReviewApprovedHeadSHA: c.Run.HeadSHA}, nil
	}}
	ex := pipeline.NewExecutor(c.DB, p, c.Config, c.Agent, []pipeline.Step{review, ci}, func(e ipc.Event) {
		if e.CIReady != nil && *e.CIReady {
			readyEvent = true
		}
	})
	ex.SetPRContextGuard(GuardPRContext)
	err := ex.Execute(ctx, c.Run, c.Repo, dir)
	if polls != 2 || !guardRejected {
		t.Fatalf("fixture did not reach retarget comparison: polls=%d rejected=%t err=%v", polls, guardRejected, err)
	}
	if gotReady || readyEvent {
		t.Fatalf("CI emitted readiness for live dev although Review and receipt validated main; eventual executor result=%v", err)
	}
}
