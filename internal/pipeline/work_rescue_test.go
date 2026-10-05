package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

func TestFixProgressRescueAgentReturnPreservesBeforeStepReturns(t *testing.T) {
	setRescueFixturePopulation(t)
	d, _, run, repo := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	head, err := git.HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	cause := errors.New("working cap reached")
	ag := &hangingAgent{name: "fixture", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 12345})
		if err := os.WriteFile(filepath.Join(opts.CWD, "unfinished.bin"), []byte{0, 255, 'C'}, 0o644); err != nil {
			t.Fatal(err)
		}
		opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 12345})
		return nil, cause
	}}
	sctx := &StepContext{Ctx: context.Background(), DB: d, Run: run, Repo: repo, Agent: ag, WorkDir: dir, Config: &config.Config{}}
	_, err = sctx.RunAgent(agent.RunOpts{CWD: dir, Purpose: "review-fix"})
	if !errors.Is(err, cause) {
		t.Fatalf("original stop lost: %v", err)
	}
	p, err := d.LatestWorkRescue(run.ID)
	if err != nil || p == nil || p.State != "saved" {
		t.Fatalf("partial work unavailable at return: %+v %v", p, err)
	}
	if err = custody.ValidatePartialWork(context.Background(), dir, p); err != nil {
		t.Fatal(err)
	}
	got, err := git.RunRaw(context.Background(), dir, "cat-file", "blob", p.Ref+":unfinished.bin")
	if err != nil || string(got) != string([]byte{0, 255, 'C'}) {
		t.Fatalf("partial bytes = %v %v", got, err)
	}
	if ag.calls != 1 {
		t.Fatalf("stop launched another repair: %d calls", ag.calls)
	}
	ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) { return &agent.Result{}, nil }
	if _, err := sctx.RunAgent(agent.RunOpts{CWD: dir, Purpose: "review-fix"}); err != nil || ag.calls != 2 {
		t.Fatalf("verified saved work blocked ordinary continuation: %v calls=%d", err, ag.calls)
	}
}

func TestRescueVerifiedFailedCleanInvocationSettles(t *testing.T) {
	setRescueFixturePopulation(t)
	d, _, run, repo := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	cause := errors.New("stopped cleanly")
	ag := &hangingAgent{name: "fixture", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 12345})
		opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 12345})
		return nil, cause
	}}
	sctx := &StepContext{Ctx: context.Background(), DB: d, Run: run, Repo: repo, Agent: ag, WorkDir: dir}
	_, err := sctx.RunAgent(agent.RunOpts{CWD: dir})
	p, readErr := d.LatestWorkRescue(run.ID)
	if !errors.Is(err, cause) || readErr != nil || p != nil {
		t.Fatalf("verified clean failure not settled: %+v %v %v", p, err, readErr)
	}
}

func TestRescueAdmissionRefusesUnsettledWork(t *testing.T) {
	for _, state := range []string{"retained", "active", "unreadable"} {
		t.Run(state, func(t *testing.T) {
			d, _, run, repo := setupTest(t)
			dir := t.TempDir()
			initGitRepo(t, dir)
			head, err := git.HeadSHA(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			p, err := d.BeginWorkRescue(run, "review", "", head, dir)
			if err != nil {
				t.Fatal(err)
			}
			if state == "retained" {
				p.State = state
				if err := d.SaveWorkRescue(p); err != nil {
					t.Fatal(err)
				}
			} else if state == "unreadable" {
				if err := d.Close(); err != nil {
					t.Fatal(err)
				}
			}
			ag := &hangingAgent{name: "fixture", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) { return &agent.Result{}, nil }}
			sctx := &StepContext{Ctx: context.Background(), DB: d, Run: run, Repo: repo, Agent: ag, WorkDir: dir}
			if _, err := sctx.RunAgent(agent.RunOpts{CWD: dir}); err == nil || ag.calls != 0 {
				t.Fatalf("unsettled state authorized editing: err=%v calls=%d", err, ag.calls)
			}
		})
	}
}

func TestRescueFailedCleanInvocationRequiresLifecycleExit(t *testing.T) {
	d, _, run, repo := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	cause := errors.New("agent stopped")
	ag := &hangingAgent{name: "fixture", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) { return nil, cause }}
	sctx := &StepContext{Ctx: context.Background(), DB: d, Run: run, Repo: repo, Agent: ag, WorkDir: dir}
	_, err := sctx.RunAgent(agent.RunOpts{CWD: dir})
	p, readErr := d.LatestWorkRescue(run.ID)
	if !errors.Is(err, cause) || readErr != nil || p == nil || p.State != "retained" || p.Path != dir {
		t.Fatalf("unknown lifecycle settled clean work: p=%+v err=%v read=%v", p, err, readErr)
	}
}

func setRescueFixturePopulation(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	population := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n-eo) printf '%d 1 %d S\\n';;\n*) printf '%d 1 %d 00:01 fixture\\n';;\nesac\n", os.Getpid(), os.Getpid(), os.Getpid(), os.Getpid())
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(population), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
