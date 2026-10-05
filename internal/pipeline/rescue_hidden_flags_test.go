//go:build unix

package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

func TestRescueHiddenFlagsStoppedAgentSavesBytes(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			setRescueFixturePopulation(t)
			d, _, run, repo := setupTest(t)
			dir := t.TempDir()
			initGitRepo(t, dir)
			file := filepath.Join(dir, "generated")
			if err := os.WriteFile(file, []byte("base\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := git.Run(context.Background(), dir, "add", "generated"); err != nil {
				t.Fatal(err)
			}
			if _, err := git.Run(context.Background(), dir, "commit", "-m", "generated"); err != nil {
				t.Fatal(err)
			}
			run.HeadSHA, _ = git.HeadSHA(context.Background(), dir)
			cause := errors.New("editing timeout")
			ag := &hangingAgent{name: "fixture", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 12345})
				if _, err := git.Run(context.Background(), dir, "update-index", flag, "generated"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte("hidden interrupted bytes\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 12345})
				return nil, cause
			}}
			sctx := &StepContext{Ctx: context.Background(), DB: d, Run: run, Repo: repo, Agent: ag, WorkDir: dir, Config: &config.Config{}}
			if _, err := sctx.RunAgent(agent.RunOpts{CWD: dir, Purpose: "test-fix"}); !errors.Is(err, cause) {
				t.Fatalf("stop lost: %v", err)
			}
			p, err := d.LatestWorkRescue(run.ID)
			if err != nil || p == nil || p.State != "saved" {
				t.Fatalf("hidden stopped bytes lost: %+v %v", p, err)
			}
			if got, err := git.RunRaw(context.Background(), dir, "cat-file", "blob", p.Ref+":generated"); err != nil || string(got) != "hidden interrupted bytes\n" {
				t.Fatalf("saved bytes=%q %v", got, err)
			}
		})
	}
}
