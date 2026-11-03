package citest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIStep_SecondMergeConflictUsesBaseCommit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		advance     bool
		unlimited   bool
		lookupFails bool
		budget      int
		wantRepairs int
	}{
		{name: "new_base", advance: true, budget: 2, wantRepairs: 2},
		{name: "new_base_unlimited_timeout", advance: true, unlimited: true, budget: 2, wantRepairs: 2},
		{name: "base_lookup_failed_during_repair", advance: true, lookupFails: true, budget: 2, wantRepairs: 1},
		{name: "same_base", budget: 2, wantRepairs: 1},
		{name: "budget_exhausted", advance: true, budget: 1, wantRepairs: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := t.TempDir()
			stepstest.GitCmd(t, upstream, "init", "--bare")
			dir := t.TempDir()
			stepstest.GitCmd(t, dir, "init", "-b", "main")
			stepstest.GitCmd(t, dir, "commit", "--allow-empty", "-m", "initial")
			baseSHA := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")
			stepstest.GitCmd(t, dir, "remote", "add", "origin", upstream)
			stepstest.GitCmd(t, dir, "push", "origin", "main")
			stepstest.GitCmd(t, dir, "checkout", "-b", "feature")
			if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644); err != nil {
				t.Fatal(err)
			}
			stepstest.GitCmd(t, dir, "add", "feature.txt")
			stepstest.GitCmd(t, dir, "commit", "-m", "feature")
			headSHA := stepstest.GitCmd(t, dir, "rev-parse", "HEAD")
			stepstest.GitCmd(t, dir, "push", "origin", "feature")
			baseDir := t.TempDir()
			stepstest.GitCmd(t, baseDir, "clone", "-b", "main", upstream, ".")
			advanceBase := func(message string) string {
				stepstest.GitCmd(t, baseDir, "commit", "--allow-empty", "-m", message)
				stepstest.GitCmd(t, baseDir, "push", "origin", "main")
				return stepstest.GitCmd(t, baseDir, "rev-parse", "HEAD")
			}
			tip := advanceBase("first base advance")
			ag := &stepstest.MockAgent{AgentName: "test", RunFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				stepstest.GitCmd(t, opts.CWD, "fetch", upstream, "main")
				stepstest.GitCmd(t, opts.CWD, "rebase", "FETCH_HEAD")
				return &agent.Result{}, nil
			}}
			sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			prURL := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &prURL
			sctx.Run.Branch = "refs/heads/feature"
			sctx.Repo.UpstreamURL = upstream
			sctx.Env = stepstest.FakeCIGHMergeable(t, "OPEN", "[]", "CONFLICTING")
			sctx.Config.CITimeout = time.Minute
			if tc.unlimited {
				sctx.Config.CITimeout = -1
			}
			sctx.Config.AutoFix = config.AutoFix{CI: tc.budget}
			step := (&steps.CIStep{}).SetBaseBranchTip(func(context.Context) (string, bool) { return tip, true }).SetWaitForNextPoll(func(context.Context, time.Duration) error { return context.Canceled })
			if tc.lookupFails {
				sctx.EvalReplay = true
				stepstest.GitCmd(t, dir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
			}
			first, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
			if err != nil || first == nil || first.RestartFrom != types.StepReview || len(ag.Calls) != 1 {
				t.Fatalf("first repair: outcome=%+v err=%v calls=%d", first, err, len(ag.Calls))
			}
			stepstest.GitCmd(t, dir, "remote", "set-url", "origin", upstream)
			sctx.EvalReplay = false
			if tc.advance {
				tip = advanceBase("second base advance")
			}
			sctx.Fixing = false
			sctx.PreviousFindings = ""
			second, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 1)
			if len(ag.Calls) != tc.wantRepairs {
				t.Fatalf("repairs=%d, want %d; outcome=%+v err=%v", len(ag.Calls), tc.wantRepairs, second, err)
			}
			switch {
			case !tc.advance || tc.lookupFails:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("same-base conflict must keep waiting: outcome=%+v err=%v", second, err)
				}
			case tc.budget == 1:
				if err != nil || second == nil || !second.NeedsApproval {
					t.Fatalf("exhausted budget must park: outcome=%+v err=%v", second, err)
				}
			default:
				if err != nil || second == nil || second.RestartFrom != types.StepReview {
					t.Fatalf("new-base conflict must repair and revalidate: outcome=%+v err=%v", second, err)
				}
			}
		})
	}
}
