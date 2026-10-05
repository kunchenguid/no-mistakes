package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestRescueRetainedConflictRefusesRollbackAndLaterEditing(t *testing.T) {
	for _, strategy := range []string{config.RebaseStrategyRebase, config.RebaseStrategyMerge} {
		t.Run(strategy, func(t *testing.T) {
			f := newMergeFixture(t, true)
			cause := errors.New("resolver stopped")
			ag := &mockAgent{name: "fixture", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 12345})
				writeFixtureFile(t, f.dir, "shared.txt", "unfinished resolution\n")
				opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 12345})
				return nil, cause
			}}
			sctx := f.context(t, ag, strategy)
			integrate := rebaseWithAgent
			if strategy == config.RebaseStrategyMerge {
				integrate = mergeWithAgent
			}
			if err := integrate(context.Background(), sctx, "origin/main"); !errors.Is(err, cause) {
				t.Fatalf("stop lost: %v", err)
			}
			if got, err := os.ReadFile(filepath.Join(f.dir, "shared.txt")); err != nil || string(got) != "unfinished resolution\n" {
				t.Fatalf("rollback discarded retained resolution: %q %v", got, err)
			}
			p, err := sctx.DB.LatestWorkRescue(sctx.Run.ID)
			if err != nil || p == nil || p.State != "retained" {
				t.Fatalf("rescue=%+v err=%v", p, err)
			}
			before := gitCmd(t, f.dir, "ls-files", "--stage")
			if err := integrate(context.Background(), sctx, "origin/main"); err == nil {
				t.Fatal("retained work admitted integration")
			}
			if _, err := sctx.RunAgent(agent.RunOpts{CWD: f.dir}); err == nil {
				t.Fatal("retained work admitted editing")
			}
			if len(ag.calls) != 1 || gitCmd(t, f.dir, "ls-files", "--stage") != before {
				t.Fatal("retained work mutated on retry")
			}
		})
	}
}

func TestRescueCIFailureDoesNotEnterOrdinaryRetry(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "fixture", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 12345})
		writeFixtureFile(t, dir, ".gitignore", "unfinished\n")
		writeFixtureFile(t, dir, "unfinished", "keep me\n")
		opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 12345})
		return nil, errors.New("fix failed")
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.PreviousFindings = `{"findings":[{"id":"ci-1","severity":"error","description":"failed","action":"auto-fix","category":"ci-check","check":"test"}]}`
	step := &CIStep{}
	host := &completionSnapshotHost{checks: []scm.Check{{Name: "test", Bucket: scm.CheckBucketFail}}}
	if outcome, err := step.repairFromFindings(sctx, host, &scm.PR{Number: "42"}); err == nil && (outcome == nil || !outcome.NeedsApproval) {
		t.Fatalf("retained repair entered ordinary retry: %+v", outcome)
	}
	if _, err := sctx.RunAgent(agent.RunOpts{CWD: dir}); err == nil || len(ag.calls) != 1 {
		t.Fatalf("retry editing: %v calls=%d", err, len(ag.calls))
	}
}

func TestRescueIncompleteSuccessfulResolverRetainsOriginalCheckout(t *testing.T) {
	for _, operation := range []string{"rebase", "merge", "merge-to-rebase"} {
		t.Run(operation, func(t *testing.T) {
			f := newMergeFixture(t, true)
			ag := &mockAgent{name: "fixture", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 12345})
				if operation == "merge-to-rebase" {
					fixtureGit(t, f.dir, "merge", "--abort")
					if err := runFixtureGit(f.dir, "rebase", "origin/main"); err == nil {
						t.Fatal("fixture rebase did not conflict")
					}
				}
				writeFixtureFile(t, f.dir, "shared.txt", "partial successful resolution\n")
				fixtureGit(t, f.dir, "add", "shared.txt")
				opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 12345})
				return &agent.Result{}, nil
			}}
			strategy := config.RebaseStrategyMerge
			integrate := mergeWithAgent
			if operation == "rebase" {
				strategy = config.RebaseStrategyRebase
				integrate = rebaseWithAgent
			}
			sctx := f.context(t, ag, strategy)
			if err := integrate(context.Background(), sctx, "origin/main"); err == nil {
				t.Fatal("incomplete resolver passed")
			}
			if got, err := os.ReadFile(filepath.Join(f.dir, "shared.txt")); err != nil || string(got) != "partial successful resolution\n" {
				t.Fatalf("incomplete resolution rolled back: %q %v", got, err)
			}
			p, err := sctx.DB.LatestWorkRescue(sctx.Run.ID)
			if err != nil || p == nil || p.State != "retained" {
				t.Fatalf("incomplete resolution lacks retention: %+v %v", p, err)
			}
			if operation == "merge" {
				if !mergeInProgress(context.Background(), f.dir) {
					t.Fatal("merge state discarded")
				}
			} else if !rebaseInProgress(context.Background(), f.dir) {
				t.Fatal("rebase state discarded")
			}
		})
	}
}

func TestRescueRejectedCompletedMergeRetainsPartialWorkBeforeReset(t *testing.T) {
	f := newMergeFixture(t, true)
	ag := &mockAgent{name: "fixture", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		fixtureGit(t, f.dir, "merge", "--abort")
		writeFixtureFile(t, f.dir, ".gitignore", "private\n")
		fixtureGit(t, f.dir, "add", ".gitignore")
		fixtureGit(t, f.dir, "commit", "-m", "unrelated change")
		writeFixtureFile(t, f.dir, "private", "unfinished ignored bytes\n")
		writeFixtureFile(t, f.dir, "shared.txt", "unfinished tracked bytes\n")
		return &agent.Result{}, nil
	}}
	sctx := f.context(t, ag, config.RebaseStrategyMerge)
	if err := mergeWithAgent(context.Background(), sctx, "origin/main"); err == nil {
		t.Fatal("invalid merge accepted")
	}
	for name, want := range map[string]string{"private": "unfinished ignored bytes\n", "shared.txt": "unfinished tracked bytes\n"} {
		if got, err := os.ReadFile(filepath.Join(f.dir, name)); err != nil || string(got) != want {
			t.Fatalf("reset discarded %s: %q %v", name, got, err)
		}
	}
	p, err := sctx.DB.LatestWorkRescue(sctx.Run.ID)
	if err != nil || p == nil || p.State != "retained" {
		t.Fatalf("partial work not retained: %+v %v", p, err)
	}
}
