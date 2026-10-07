package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type retainedConclusionStep struct {
	*CIStep
	f        *ciRepairFixture
	outcomes chan *pipeline.StepOutcome
}

func (s *retainedConclusionStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.f.sctx.Ctx = ctx.Ctx
	s.f.sctx.Run = ctx.Run
	s.f.sctx.Fixing = ctx.Fixing
	if ctx.PreviousFindings != "" {
		s.f.sctx.PreviousFindings = ctx.PreviousFindings
	}
	s.f.sctx.DeferredFindings = ctx.DeferredFindings
	out, err := s.repairFromFindings(s.f.sctx, &completionSnapshotHost{checks: []scm.Check{{Name: "test", ProviderID: "github-check-run:42", Bucket: scm.CheckBucketFail, CompletedAt: time.Now()}}}, &scm.PR{Number: "42"})
	if out != nil {
		s.outcomes <- out
	}
	return out, err
}
func (s *retainedConclusionStep) ReconcileApprovalGate(ctx *pipeline.StepContext) (bool, error) {
	ctx.Env = s.f.sctx.Env
	return s.CIStep.ReconcileApprovalGate(ctx)
}

type retainedReviewBoundary struct{ submitted string }

func (s retainedReviewBoundary) Name() types.StepName { return types.StepReview }
func (s retainedReviewBoundary) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if ctx.Run.HeadSHA == s.submitted {
		return &pipeline.StepOutcome{ReviewApprovedHeadSHA: s.submitted}, nil
	}
	return &pipeline.StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"review-required","severity":"warning","description":"retained repair awaits fresh review","action":"ask-user"}]}`}, nil
}

func retainedRepairSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	state := map[string]string{"HEAD": gitCmd(t, dir, "rev-parse", "HEAD"), "entries": gitCmd(t, dir, "ls-files", "--stage")}
	index := gitCmd(t, dir, "rev-parse", "--git-path", "index")
	if !filepath.IsAbs(index) {
		index = filepath.Join(dir, index)
	}
	for name, path := range map[string]string{"index": index, "repair.txt": filepath.Join(dir, "repair.txt"), "feature.txt": filepath.Join(dir, "feature.txt"), "init.txt": filepath.Join(dir, "init.txt")} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		state[name] = string(raw)
	}
	return state
}

func TestCIRepair_RetainedCommitConclusionThroughExecutor(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		for _, revalidate := range []bool{false, true} {
			for _, accept := range []bool{false, true} {
				name := fmt.Sprintf("recovered=%t/revalidate=%t/accept=%t", recovered, revalidate, accept)
				t.Run(name, func(t *testing.T) {
					f := newUnconcludedFixture(t, false, func(string) {}, `{}`)
					f.sctx.Config.CI.RevalidateRepairs = revalidate
					if err := f.sctx.DB.UpdateRunPRURL(f.sctx.Run.ID, *f.sctx.Run.PRURL); err != nil {
						t.Fatal(err)
					}
					calls := 0
					f.sctx.Agent = &mockAgent{name: "two-round", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
						calls++
						if calls == 1 {
							if err := os.WriteFile(filepath.Join(opts.CWD, "repair.txt"), []byte("retained\x00repair\n"), 0600); err != nil {
								return nil, err
							}
							gitCmd(t, opts.CWD, "add", "repair.txt")
							gitCmd(t, opts.CWD, "commit", "-m", "stopped local repair")
							return &agent.Result{Output: json.RawMessage(`{"summary":"stopped after local commit","code_change_needed":true,"stopped":true}`)}, nil
						}
						if calls != 2 {
							return nil, fmt.Errorf("unexpected repair invocation %d", calls)
						}
						return &agent.Result{Output: json.RawMessage(fmt.Sprintf(`{"summary":"no additional edits","code_change_needed":%t}`, accept))}, nil
					}}
					p := paths.WithRoot(t.TempDir())
					if err := p.EnsureDirs(); err != nil {
						t.Fatal(err)
					}
					step := &retainedConclusionStep{CIStep: &CIStep{}, f: f, outcomes: make(chan *pipeline.StepOutcome, 2)}
					if recovered {
						if err := f.sctx.DB.UpdateRunStatus(f.sctx.Run.ID, types.RunRunning); err != nil {
							t.Fatal(err)
						}
						review, err := f.sctx.DB.InsertStepResult(f.sctx.Run.ID, types.StepReview)
						if err != nil {
							t.Fatal(err)
						}
						if err := f.sctx.DB.StartStep(review.ID); err != nil {
							t.Fatal(err)
						}
						if err := f.sctx.DB.CompleteReviewStep(review.ID, f.sctx.Run.ID, f.headSHA, 0, 1, ""); err != nil {
							t.Fatal(err)
						}
						out := f.repairRound(t)
						if out == nil || !out.NeedsApproval || out.RestartFrom != "" {
							t.Fatalf("first stop outcome=%#v", out)
						}
						sr, err := f.sctx.DB.InsertStepResult(f.sctx.Run.ID, types.StepCI)
						if err != nil {
							t.Fatal(err)
						}
						if err := f.sctx.DB.StartStep(sr.ID); err != nil {
							t.Fatal(err)
						}
						if _, err := f.sctx.DB.InsertStepRound(sr.ID, 1, "initial", &out.Findings, nil, 1); err != nil {
							t.Fatal(err)
						}
						if err := f.sctx.DB.ParkStepForApproval(f.sctx.Run.ID, sr.ID, types.StepStatusAwaitingApproval, 0, 1, &out.Findings); err != nil {
							t.Fatal(err)
						}
						f.sctx.Run, err = f.sctx.DB.GetRun(f.sctx.Run.ID)
						if err != nil {
							t.Fatal(err)
						}
					}
					parks := make(chan types.StepName, 4)
					executor := pipeline.NewExecutor(f.sctx.DB, p, f.sctx.Config, f.sctx.Agent, []pipeline.Step{retainedReviewBoundary{submitted: f.headSHA}, step}, func(event ipc.Event) {
						if event.Type == ipc.EventStepCompleted && event.Status != nil && (*event.Status == string(types.StepStatusAwaitingApproval) || *event.Status == string(types.StepStatusFixReview)) && event.StepName != nil {
							parks <- *event.StepName
						}
					})
					executor.SetGateReconcileTimings(time.Hour, 0)
					ctx, cancel := context.WithCancel(context.Background())
					done := make(chan error, 1)
					go func() {
						defer close(done)
						if recovered {
							done <- executor.Resume(ctx, f.sctx.Run, f.sctx.Repo, f.dir)
						} else {
							done <- executor.Execute(ctx, f.sctx.Run, f.sctx.Repo, f.dir)
						}
					}()
					defer func() {
						cancel()
						select {
						case <-done:
						case <-time.After(10 * time.Second):
							t.Error("executor failed to cancel")
						}
					}()
					waitPark := func() types.StepName {
						select {
						case parked := <-parks:
							return parked
						case err := <-done:
							t.Fatalf("executor ended before park: %v", err)
						case <-time.After(10 * time.Second):
							t.Fatal("executor did not park")
						}
						return ""
					}
					if parked := waitPark(); parked != types.StepCI {
						t.Fatalf("first park=%s", parked)
					}
					if !recovered {
						out := <-step.outcomes
						if out.RestartFrom != "" || !out.NeedsApproval {
							t.Fatalf("first stop=%#v", out)
						}
					}
					before := retainedRepairSnapshot(t, f.dir)
					run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if calls != 1 || run.HeadSHA != before["HEAD"] || run.HeadSHA == f.headSHA || run.LastPushedSHA == nil || *run.LastPushedSHA != f.headSHA || run.ReviewApprovedHeadSHA != nil || f.remoteHead(t) != f.headSHA {
						t.Fatalf("first stop custody=%+v calls=%d", run, calls)
					}
					binding := []any{run.SubmittedHeadSHA, run.LastPushedSHA, run.PushTargetKind, run.PushTargetFingerprint, run.PushRef, run.LastPushedAt, run.PushGeneration}
					t.Logf("first stop: local=%s durable=%s published=%s remote=%s", before["HEAD"], run.HeadSHA, *run.LastPushedSHA, f.remoteHead(t))
					if err := executor.Respond(types.StepCI, types.ActionFix, []string{"ci-1"}); err != nil {
						t.Fatal(err)
					}
					var second *pipeline.StepOutcome
					select {
					case second = <-step.outcomes:
					case err := <-done:
						t.Fatalf("executor ended before second outcome: %v", err)
					case <-time.After(10 * time.Second):
						t.Fatal("no second repair outcome")
					}
					if accept {
						if second.RestartFrom != types.StepReview || second.NeedsApproval {
							t.Fatalf("explicit acceptance did not route retained commit to Review: %#v", second)
						}
					} else if second.RestartFrom != "" || !second.NeedsApproval || second.RepairPublished || second.AutoFixable {
						t.Errorf("false conclusion implicitly accepted retained commit: %#v", second)
					}
					wantPark := types.StepCI
					if accept {
						wantPark = types.StepReview
					}
					secondPark := waitPark()
					if secondPark != wantPark {
						t.Errorf("second park=%s, want %s", secondPark, wantPark)
					}
					if !accept {
						findings, err := types.ParseFindingsJSON(second.Findings)
						if err != nil || len(findings.Items) != 1 || findings.Items[0].ID != "ci-1" || findings.Items[0].ActionOrDefault() != types.ActionAskUser {
							t.Errorf("false conclusion lost explicit gate findings: %+v %v", findings, err)
						}
						parkedRun, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
						if err != nil || parkedRun.AwaitingAgentSince == nil {
							t.Errorf("false conclusion has no durable park: %+v %v", parkedRun, err)
						}
					}
					cancel()
					select {
					case <-done:
					case <-time.After(10 * time.Second):
						t.Fatal("executor did not cancel")
					}
					run, err = f.sctx.DB.GetRun(f.sctx.Run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if calls != 2 || run.HeadSHA != before["HEAD"] || run.LastPushedSHA == nil || *run.LastPushedSHA != f.headSHA || run.ReviewApprovedHeadSHA != nil || f.remoteHead(t) != f.headSHA {
						t.Fatalf("second round custody=%+v calls=%d", run, calls)
					}
					if !reflect.DeepEqual(binding, []any{run.SubmittedHeadSHA, run.LastPushedSHA, run.PushTargetKind, run.PushTargetFingerprint, run.PushRef, run.LastPushedAt, run.PushGeneration}) {
						t.Fatal("second conclusion changed push/submission binding")
					}
					if after := retainedRepairSnapshot(t, f.dir); !reflect.DeepEqual(before, after) {
						t.Fatal("no-edit response changed HEAD/raw index/bytes")
					}
					t.Logf("second conclusion: accepted=%t parked=%s restart=%s local=%s durable=%s published=%s remote=%s", accept, secondPark, second.RestartFrom, before["HEAD"], run.HeadSHA, *run.LastPushedSHA, f.remoteHead(t))
				})
			}
		}
	}
}

func TestCIRepair_CleanNoChangeHasNoUnpublishedWork(t *testing.T) {
	f := newUnconcludedFixture(t, false, func(string) {}, `{"summary":"external failure","code_change_needed":false,"stopped":null}`)
	out := f.repairRound(t)
	if out == nil || !out.NeedsApproval || out.RestartFrom != "" || out.RepairPublished || f.localHead(t) != f.headSHA || f.remoteHead(t) != f.headSHA {
		t.Fatalf("clean no-change=%#v", out)
	}
	findings, err := types.ParseFindingsJSON(out.Findings)
	if err != nil || findings.Summary != "external failure" {
		t.Fatalf("ordinary no-change findings=%+v, err=%v", findings, err)
	}
}
