package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type parkedRestartCI struct {
	*CIStep
	target string
	parked bool
}

func (s *parkedRestartCI) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if s.parked {
		return &pipeline.StepOutcome{}, nil
	}
	s.parked = true
	if err := sctx.DB.UpdateRunHeadSHAForRevalidation(sctx.Run.ID, s.target); err != nil {
		return nil, err
	}
	if _, err := stepGitRun(sctx, "reset", "--hard", s.target); err != nil {
		return nil, err
	}
	sctx.Run.HeadSHA = s.target
	sctx.Run.ReviewApprovedHeadSHA = nil
	return &pipeline.StepOutcome{
		RestartFrom:   types.StepReview,
		NeedsApproval: true,
		Findings:      `{"findings":[{"id":"deferred","severity":"warning","action":"ask-user","description":"deferred CI decision"}]}`,
	}, nil
}

type restartBaseReview struct {
	t     *testing.T
	calls int
}

func (s *restartBaseReview) Name() types.StepName { return types.StepReview }
func (s *restartBaseReview) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.calls++
	want := "layer1"
	if s.calls > 1 {
		want = "main"
	}
	if got := effectivePRBaseBranch(sctx); got != want {
		s.t.Errorf("Review %d scoped to %q, want %q", s.calls, got, want)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || run.PRBaseBranch == nil || *run.PRBaseBranch != want {
		s.t.Errorf("Review %d durable base = %+v, %v; want %q", s.calls, run, err, want)
	}
	return &pipeline.StepOutcome{ReviewApprovedHeadSHA: sctx.Run.HeadSHA}, nil
}

func TestCIStep_RestartRefreshesBaseAfterParkWithoutPush(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionFix, types.ActionSkip} {
			name := string(action)
			if recovered {
				name += "/recovered"
			} else {
				name += "/live"
			}
			t.Run(name, func(t *testing.T) {
				f := newCIRepairFixture(t, false, nil)
				run, err := f.sctx.DB.InsertRun(f.sctx.Repo.ID, f.sctx.Run.Branch, f.headSHA, f.sctx.Run.BaseSHA)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.sctx.DB.UpdateRunPRURL(run.ID, *f.sctx.Run.PRURL); err != nil {
					t.Fatal(err)
				}
				if err := f.sctx.DB.UpdateRunPRBaseBranch(run.ID, "layer1"); err != nil {
					t.Fatal(err)
				}
				initial := "layer1"
				run.PRBaseBranch = &initial
				run.PRURL = f.sctx.Run.PRURL
				target := gitCmd(t, f.dir, "commit-tree", gitCmd(t, f.dir, "rev-parse", "HEAD^{tree}"), "-p", f.headSHA, "-m", "rewritten")
				baseFile := filepath.Join(t.TempDir(), "live-base")
				if err := os.WriteFile(baseFile, []byte("layer1"), 0o644); err != nil {
					t.Fatal(err)
				}
				env := append(f.sctx.Env, "FAKE_CLI_PR_BASE_FILE="+baseFile)
				review := &restartBaseReview{t: t}
				ci := &parkedRestartCI{CIStep: &CIStep{}, target: target}
				steps := []pipeline.Step{review, reconcileEnvStep{step: ci, env: env}}
				parked := make(chan struct{}, 1)
				p := paths.WithRoot(t.TempDir())
				newExecutor := func() *pipeline.Executor {
					return pipeline.NewExecutor(f.sctx.DB, p, &config.Config{}, nil, steps, func(event ipc.Event) {
						if event.Type == ipc.EventStepCompleted && event.Status != nil && (*event.Status == string(types.StepStatusAwaitingApproval) || *event.Status == string(types.StepStatusFixReview)) {
							select {
							case parked <- struct{}{}:
							default:
							}
						}
					})
				}
				executor := newExecutor()
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(context.Canceled)
				done := make(chan error, 1)
				go func() { done <- executor.Execute(ctx, run, f.sctx.Repo, f.dir) }()
				select {
				case <-parked:
				case <-time.After(10 * time.Second):
					t.Fatal("CI never parked")
				}
				if recovered {
					cancel(pipeline.ErrDaemonShutdown)
					if err := <-done; !errors.Is(err, pipeline.ErrParkedAtShutdown) {
						t.Fatalf("shutdown = %v", err)
					}
					run, err = f.sctx.DB.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					executor = newExecutor()
					ctx, cancel = context.WithCancelCause(context.Background())
					defer cancel(context.Canceled)
					go func() { done <- executor.Resume(ctx, run, f.sctx.Repo, f.dir) }()
					select {
					case <-parked:
					case <-time.After(10 * time.Second):
						t.Fatal("recovered CI never parked")
					}
				}
				if err := os.WriteFile(baseFile, []byte("main"), 0o644); err != nil {
					t.Fatal(err)
				}
				var ids []string
				if action == types.ActionFix {
					ids = []string{"deferred"}
				}
				if err := executor.Respond(types.StepCI, action, ids); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("restart did not finish")
				}
				if review.calls != 2 {
					t.Fatalf("Review ran %d times, want two", review.calls)
				}
				run, err = f.sctx.DB.GetRun(run.ID)
				if err != nil || run.LastPushedSHA != nil || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != target {
					t.Fatalf("restart lost validation or required publication: %+v, %v", run, err)
				}
			})
		}
	}
}

func TestCIStep_RestartRefusesAnUnreadableLiveBase(t *testing.T) {
	for _, base := range []string{"!", "-", "bad..base"} {
		t.Run(base, func(t *testing.T) {
			f := newCIRepairFixture(t, false, nil)
			f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_PR_BASE="+base)
			if err := (&CIStep{}).PrepareRestart(f.sctx, types.StepReview); err == nil {
				t.Fatal("Review restart accepted an unreadable or invalid live base")
			}
			run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil || run.PRBaseBranch != nil || f.sctx.Run.PRBaseBranch != nil {
				t.Fatalf("refused base changed restart scope: %+v, %v", run, err)
			}
		})
	}
}
