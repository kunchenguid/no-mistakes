package steps

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type mergedReadGateStep struct {
	reconcileEnvStep
	attempts chan error
}

func (s mergedReadGateStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return ciMonitoringTimeoutOutcome(), nil
}

func (s mergedReadGateStep) ReconcileApprovalGate(sctx *pipeline.StepContext) (bool, error) {
	resolved, err := s.reconcileEnvStep.ReconcileApprovalGate(sctx)
	select {
	case s.attempts <- err:
	default:
	}
	return resolved, err
}

// The operator's contract distinguishes an unavailable read from a successful
// response that cannot identify the merged head. Exercise the real reconciler
// and executor: wrapping a 503 as a deterministic refusal would fail the run
// before its second attempt; accepting the later empty response would certify
// a merge at a head the run never validated.
func TestCIStep_MergedHeadReadRetriesThenRefusesUnidentifiableHead(t *testing.T) {
	t.Parallel()
	for _, fallbackUnavailable := range []bool{false, true} {
		name := "primary failure with successful unbound fallback"
		if fallbackUnavailable {
			name = "both reads fail"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var unavailable atomic.Bool
			unavailable.Store(true)
			var prReads atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/2.0/repositories/test/repo/pullrequests/42":
					// Each attempt reads state first, then the head. Keep state readable
					// so the outage reaches the shared merged-head boundary.
					if prReads.Add(1)%2 == 0 && unavailable.Load() {
						http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
						return
					}
					if _, writeErr := fmt.Fprint(w, `{"id":42,"state":"MERGED"}`); writeErr != nil {
						t.Error(writeErr)
					}
				case "/2.0/repositories/test/repo/pullrequests/42/statuses":
					if fallbackUnavailable && unavailable.Load() {
						http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
						return
					}
					if _, writeErr := fmt.Fprint(w, `{"values":[]}`); writeErr != nil {
						t.Error(writeErr)
					}
				default:
					t.Errorf("unexpected API request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
			sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
			prURL := "https://bitbucket.org/test/repo/pull-requests/42"
			sctx.Run.PRURL = &prURL
			step := mergedReadGateStep{
				reconcileEnvStep: reconcileEnvStep{step: &CIStep{}, env: fakeBitbucketEnv(server.URL)},
				attempts:         make(chan error, 4),
			}
			executor := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, nil, []pipeline.Step{step}, nil)
			executor.SetGateReconcileTimings(10*time.Millisecond, 5*time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- executor.Execute(ctx, sctx.Run, sctx.Repo, dir) }()
			for range 2 {
				select {
				case readErr := <-step.attempts:
					if readErr == nil || errors.Is(readErr, pipeline.ErrFatalGateReconciliation) {
						t.Fatalf("merged-head read = %v, want a retryable failure", readErr)
					}
				case <-ctx.Done():
					t.Fatal("merged-head read was not retried")
				}
			}
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != types.RunRunning || run.AwaitingAgentSince == nil || !executor.AwaitingApproval() {
				t.Fatalf("read outage changed parked run: status=%s awaiting=%v", run.Status, run.AwaitingAgentSince)
			}
			unavailable.Store(false)
			select {
			case result := <-done:
				if !errors.Is(result, pipeline.ErrFatalGateReconciliation) || !errors.Is(result, errMergedHeadUnproven) {
					t.Fatalf("successful unidentifiable read = %v, want a fatal refusal", result)
				}
			case <-ctx.Done():
				t.Fatal("unidentifiable merged head did not fail the run")
			}
			run, err = sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != types.RunFailed || run.AwaitingAgentSince != nil || executor.AwaitingApproval() {
				t.Fatalf("unidentifiable merge left run parked: status=%s awaiting=%v", run.Status, run.AwaitingAgentSince)
			}
		})
	}
}

func TestCIStep_MergedHeadPollingRetriesPrimaryReadFailure(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = time.Minute
	var recovered atomic.Bool
	var prReads atomic.Int64
	var fallbackReads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/2.0/repositories/test/repo/pullrequests/42":
			// Execute reads the base before polling state, then verifies the
			// merged head. Fail that third read, leaving state readable.
			if prReads.Add(1) == 3 && !recovered.Load() {
				http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			if _, writeErr := fmt.Fprintf(w, `{"id":42,"state":"MERGED","source":{"commit":{"hash":%q}}}`, head); writeErr != nil {
				t.Error(writeErr)
			}
		case "/2.0/repositories/test/repo/pullrequests/42/statuses":
			fallbackReads.Add(1)
			if _, writeErr := fmt.Fprint(w, `{"values":[]}`); writeErr != nil {
				t.Error(writeErr)
			}
		default:
			t.Errorf("unexpected API request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	sctx.Env = fakeBitbucketEnv(server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sctx.Ctx = ctx
	waits := 0
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
		waits++
		recovered.Store(true)
		return nil
	}}
	outcome, err := step.Execute(sctx)
	if err != nil || outcome == nil || waits != 1 || fallbackReads.Load() != 1 {
		t.Fatalf("poll = %#v, %v; waits=%d fallback reads=%d, want retry then verified merge", outcome, err, waits, fallbackReads.Load())
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRState == nil || *run.PRState != "merged" || run.Status == types.RunFailed {
		t.Fatalf("recovered merge state = %+v", run)
	}
}
