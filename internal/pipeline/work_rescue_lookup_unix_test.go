//go:build unix

package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

func TestRescueLookupFailureRetainsCleanAndDirtyInvocation(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "dirty"}[dirty], func(t *testing.T) {
			d, _, run, repo := setupTest(t)
			dir := t.TempDir()
			initGitRepo(t, dir)
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			ag := &hangingAgent{name: "fixture", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 12345})
				if dirty {
					if err := os.WriteFile(filepath.Join(dir, "unfinished"), []byte("keep me"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 12345})
				return nil, errors.New("stopped")
			}}
			sctx := &StepContext{Ctx: context.Background(), DB: d, Run: run, Repo: repo, Agent: ag, WorkDir: dir}
			_, err := sctx.RunAgent(agent.RunOpts{CWD: dir})
			p, readErr := d.LatestWorkRescue(run.ID)
			if err == nil || readErr != nil || p == nil || p.State != "retained" || p.Path != dir {
				t.Fatalf("unverified lookup settled work: %+v err=%v read=%v", p, err, readErr)
			}
		})
	}
}
