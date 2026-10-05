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
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func TestRescueRawSettlementCompletedRebaseTimeoutRetainsRewrittenHead(t *testing.T) {
	f := newMergeFixture(t, true)
	setRescueFixturePopulation(t)
	gitCmd(t, f.dir, "checkout", "--detach", f.headSHA)
	var invocationHead string
	ag := &mockAgent{name: "fixture", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 12345})
		invocationHead = gitCmd(t, f.dir, "rev-parse", "HEAD")
		writeFixtureFile(t, f.dir, "shared.txt", "resolved private bytes\n")
		fixtureGit(t, f.dir, "add", "shared.txt")
		fixtureGit(t, f.dir, "-c", "core.editor=true", "rebase", "--continue")
		<-ctx.Done()
		opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 12345})
		return nil, ctx.Err()
	}}
	sctx := f.context(t, ag, config.RebaseStrategyRebase)
	sctx.Config.AgentTimeout = 100 * time.Millisecond
	err := rebaseWithAgent(context.Background(), sctx, "origin/main")
	if !errors.Is(err, pipeline.ErrWorkRetained) {
		t.Errorf("rewritten head settled after resolver timeout: %v", err)
	}
	rewritten := gitCmd(t, f.dir, "rev-parse", "HEAD")
	if rewritten == f.headSHA || rebaseInProgress(context.Background(), f.dir) {
		t.Fatal("fixture did not finish actual rebase")
	}
	if _, err := git.Run(context.Background(), f.dir, "merge-base", "--is-ancestor", f.headSHA, rewritten); err == nil {
		t.Fatal("fixture rewrite is a descendant")
	}
	p, err := sctx.DB.LatestWorkRescue(sctx.Run.ID)
	if err != nil || p == nil || p.State != "retained" || p.ParentHead != invocationHead || p.Path != f.dir {
		t.Fatalf("rewritten detached worktree not retained: %+v %v", p, err)
	}
	if err := sctx.CheckWorkRescue(); !errors.Is(err, pipeline.ErrWorkRetained) {
		t.Errorf("later mutation accepted: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(f.dir, "shared.txt")); err != nil || string(got) != "resolved private bytes\n" {
		t.Fatal("resolved bytes lost")
	}
}

func TestRescueRawSettlementTestTimeoutSavesBeforePreparation(t *testing.T) {
	setRescueFixturePopulation(t)
	dir, base, head := setupGitRepo(t)
	writeFixtureFile(t, dir, "base.txt", "base\n")
	gitCmd(t, dir, "add", "base.txt")
	gitCmd(t, dir, "commit", "-m", "text fixture")
	head = gitCmd(t, dir, "rev-parse", "HEAD")
	attributes := filepath.Join(t.TempDir(), "attributes")
	if err := os.WriteFile(attributes, []byte("* text eol=crlf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "config", "core.attributesfile", attributes)
	ag := &mockAgent{name: "fixture", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseStart, PID: 12345})
		writeFixtureFile(t, dir, "base.txt", "base\r\n")
		gitCmd(t, dir, "add", "base.txt")
		status, err := git.RunWithEnv(context.Background(), dir, []string{"GIT_OPTIONAL_LOCKS=1"}, "status", "--porcelain")
		if err != nil || status != "" {
			diff, _ := git.Run(context.Background(), dir, "diff", "--", "base.txt")
			t.Fatalf("normalization fixture status=%q error=%v diff=%s", status, err, diff)
		}
		<-ctx.Done()
		opts.OnLifecycle(agent.LifecycleEvent{Phase: agent.LifecyclePhaseExit, PID: 12345})
		return nil, ctx.Err()
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Prepare: "echo prepared > prepared", Test: "echo tested > tested"})
	sctx.GateDir = t.TempDir()
	gitCmd(t, sctx.GateDir, "init", "--bare")
	sctx.Fixing = true
	sctx.Config.TestAgentTimeout = 50 * time.Millisecond
	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatalf("verified saved fallback refused: %v", err)
	}
	p, err := sctx.DB.LatestWorkRescue(sctx.Run.ID)
	if err != nil || p == nil || p.State != "saved" {
		t.Fatalf("normalized bytes lost before preparation: %+v %v", p, err)
	}
	if got, err := git.RunRaw(context.Background(), dir, "cat-file", "blob", p.Ref+":base.txt"); err != nil || string(got) != "base\r\n" {
		t.Fatalf("saved bytes=%q %v", got, err)
	}
}
