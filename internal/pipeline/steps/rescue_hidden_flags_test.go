//go:build unix

package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

func TestRescueHiddenFlagsBeforeResetAndTimeoutFallback(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		for _, consumer := range []string{"reset", "timeout"} {
			t.Run(flag+"/"+consumer, func(t *testing.T) {
				setRescueFixturePopulation(t)
				dir, base, head := setupGitRepo(t)
				hide := func() {
					t.Helper()
					gitCmd(t, dir, "update-index", flag, "base.txt")
					writeFixtureFile(t, dir, "base.txt", "hidden step bytes\n")
				}
				ag := &mockAgent{name: "fixture", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 12345})
					hide()
					<-ctx.Done()
					opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 12345})
					return nil, ctx.Err()
				}}
				sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Prepare: "echo prepared > prepared", Test: "echo tested > tested"})
				sctx.GateDir = t.TempDir()
				gitCmd(t, sctx.GateDir, "init", "--bare")
				if consumer == "reset" {
					hide()
					cause := errors.New("rejected merge")
					if err := restorePreMergeHead(context.Background(), sctx, head, cause); !errors.Is(err, cause) {
						t.Fatalf("rejection lost: %v", err)
					}
				} else {
					sctx.Fixing = true
					sctx.Config.TestAgentTimeout = 50 * time.Millisecond
					if _, err := (&TestStep{}).Execute(sctx); err != nil {
						t.Fatalf("verified saved timeout fallback refused: %v", err)
					}
					if _, err := os.Stat(filepath.Join(dir, "tested")); err != nil {
						t.Fatalf("configured fallback did not run: %v", err)
					}
				}
				saved, err := sctx.DB.LatestWorkRescue(sctx.Run.ID)
				if err != nil || saved == nil || saved.State != "saved" {
					t.Fatalf("step lost hidden bytes before mutation: %+v %v", saved, err)
				}
				if got, err := git.RunRaw(context.Background(), dir, "cat-file", "blob", saved.Ref+":base.txt"); err != nil || string(got) != "hidden step bytes\n" {
					t.Fatalf("rescued bytes=%q %v", got, err)
				}
			})
		}
	}
}
