package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const behindBaseGreenChecks = `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"test","state":"SUCCESS","bucket":"pass"}]`

// behindBaseContext is a GitHub PR with green checks whose mergeability is
// the given `<mergeable> <mergeStateStatus>` answer.
func behindBaseContext(t *testing.T, ag *mockAgent, mergeable string) (*pipeline.StepContext, *[]string) {
	t.Helper()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, upstream, "config", "gc.auto", "0")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	recordReviewApproval(t, sctx, headSHA)
	sctx.Env = fakeCIGHMergeable(t, "OPEN", behindBaseGreenChecks, mergeable)
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}
	logs := &[]string{}
	sctx.Log = func(s string) { *logs = append(*logs, s) }
	return sctx, logs
}

func countLogs(logs []string, line string) int {
	n := 0
	for _, l := range logs {
		if l == line {
			n++
		}
	}
	return n
}

// A green PR GitHub reports BEHIND is refused by a base that requires
// up-to-date branches, so the monitor takes the merge-conflict rebase repair
// once the base has held still for one poll, and only once (issue #1402).
func TestCIStep_GreenBehindPRTakesTheRebaseRepairOnceAfterTheBaseSettles(t *testing.T) {
	t.Parallel()
	var prompts []string
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompts = append(prompts, opts.Prompt)
		// The repair is a real clean rebase onto the advanced base.
		gitCmd(t, opts.CWD, "fetch", "origin", "main")
		gitCmd(t, opts.CWD, "rebase", "origin/main")
		return &agent.Result{}, nil
	}}
	sctx, logs := behindBaseContext(t, ag, "MERGEABLE BEHIND")
	originalHead := sctx.Run.HeadSHA
	// The real upstream base advances after the push, which is what makes the PR BEHIND.
	other := t.TempDir()
	gitCmd(t, other, "clone", "-b", "main", sctx.Repo.UpstreamURL, ".")
	gitCmd(t, other, "config", "user.name", "test")
	gitCmd(t, other, "config", "user.email", "test@test.com")
	if err := os.WriteFile(filepath.Join(other, "base-advance.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, other, "add", "-A")
	gitCmd(t, other, "commit", "-m", "base advances")
	gitCmd(t, other, "push", "origin", "main")
	advancedBase := gitCmd(t, other, "rev-parse", "HEAD")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	polls := 0
	step := &CIStep{
		baseBranchTip: func(context.Context) (string, bool) { return "base-tip-1", true },
		waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
			polls++
			if len(prompts) > 0 || polls > 5 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	outcome, err := driveCI(t, step, sctx)
	if err != nil {
		t.Fatalf("driveCI: %v", err)
	}

	if len(prompts) != 1 {
		t.Fatalf("rebase repair ran %d times, want exactly once; logs: %v", len(prompts), *logs)
	}
	if !strings.Contains(prompts[0], "behind a base branch that requires branches to be up to date") || !strings.Contains(prompts[0], "rebase target commit:") {
		t.Fatalf("repair prompt does not ask for the rebase onto the base:\n%s", prompts[0])
	}
	// A rebased head no longer descends from the reviewed one, so the repair
	// revalidates from Review and re-pushes through Push.
	if outcome == nil || outcome.RestartFrom != types.StepReview {
		t.Fatalf("outcome = %+v, want a restart from Review", outcome)
	}
	if polls != 1 {
		t.Fatalf("polls before the repair = %d, want 1 (debounced); logs: %v", polls, *logs)
	}
	// The rewritten head sits on the advanced base, stays local, and has lost
	// its review approval, so Push must re-approve it before publishing.
	newHead := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
	if newHead == originalHead {
		t.Fatalf("head was not rewritten by the rebase; logs: %v", *logs)
	}
	gitCmd(t, sctx.WorkDir, "merge-base", "--is-ancestor", advancedBase, newHead)
	if sctx.Run.HeadSHA != newHead {
		t.Fatalf("run head = %s, want the rebased head %s", sctx.Run.HeadSHA, newHead)
	}
	if remote := gitCmd(t, sctx.WorkDir, "ls-remote", "origin", "refs/heads/feature"); !strings.HasPrefix(remote, originalHead) {
		t.Fatalf("rebased head was pushed; remote feature = %q, want %s", remote, originalHead)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ReviewApprovedHeadSHA != nil && strings.TrimSpace(*run.ReviewApprovedHeadSHA) != "" {
		t.Fatalf("review approval survived the rebase: %s", *run.ReviewApprovedHeadSHA)
	}
	if got := countLogs(*logs, ciChecksPassedMsg); got != 0 {
		t.Fatalf("a BEHIND PR was reported as checks passed; logs: %v", *logs)
	}
}

// The first BEHIND observation is a wait, the second at the same base tip is
// one auto-fix finding the executor routes into the rebase repair.
func TestCIStep_GreenBehindPRObservationIsOneBehindBaseFinding(t *testing.T) {
	t.Parallel()
	sctx, logs := behindBaseContext(t, &mockAgent{name: "test"}, "MERGEABLE BEHIND")
	polls := 0
	step := &CIStep{
		baseBranchTip: func(context.Context) (string, bool) { return "base-tip-1", true },
		waitForNextPoll: func(context.Context, time.Duration) error {
			polls++
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if polls != 1 {
		t.Fatalf("polls before the rebase finding = %d, want 1 (debounced); logs: %v", polls, *logs)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Category != types.FindingCategoryCIBehindBase || findings.Items[0].Action != types.ActionAutoFix {
		t.Fatalf("findings = %+v, want one auto-fix %s finding", findings.Items, types.FindingCategoryCIBehindBase)
	}
	targets, err := parseCIFixTargets(outcome.Findings)
	if err != nil || !targets.BehindBase || targets.MergeConflict {
		t.Fatalf("fix targets = %+v (err %v), want BehindBase only", targets, err)
	}
}

// A base that advances on every poll never settles, so the monitor keeps
// waiting instead of rebasing a head that is stale again at once.
func TestCIStep_GreenBehindPROnAMovingBaseIsNotRebased(t *testing.T) {
	t.Parallel()
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Error("agent ran while the base kept moving")
		return &agent.Result{}, nil
	}}
	sctx, logs := behindBaseContext(t, ag, "MERGEABLE BEHIND")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx
	tips := 0
	polls := 0
	step := &CIStep{
		baseBranchTip: func(context.Context) (string, bool) {
			tips++
			return "base-tip-" + itoaTest(tips), true
		},
		waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
			polls++
			if polls >= 4 {
				cancel()
				return ctx.Err()
			}
			return nil
		},
	}
	driveCI(t, step, sctx)
	if got := countLogs(*logs, ciBehindBaseMsg); got != 1 {
		t.Fatalf("debounce log seen %d times, want 1 (deduplicated); logs: %v", got, *logs)
	}
}

// A PR GitHub does not report BEHIND - including one that is behind a base
// with no up-to-date rule, which GitHub reports CLEAN - is never rebased.
func TestCIStep_GreenPRThatIsNotBehindIsNeverRebased(t *testing.T) {
	t.Parallel()
	for _, mergeable := range []string{"MERGEABLE CLEAN", "MERGEABLE BLOCKED", "MERGEABLE"} {
		t.Run(mergeable, func(t *testing.T) {
			t.Parallel()
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				t.Error("agent ran for a PR that is not BEHIND")
				return &agent.Result{}, nil
			}}
			sctx, logs := behindBaseContext(t, ag, mergeable)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sctx.Ctx = ctx
			polls := 0
			step := &CIStep{
				baseBranchTip: func(context.Context) (string, bool) { return "base-tip-1", true },
				waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
					polls++
					if polls >= 3 {
						cancel()
						return ctx.Err()
					}
					return nil
				},
			}
			driveCI(t, step, sctx)
			if countLogs(*logs, ciChecksPassedMsg) != 1 || countLogs(*logs, ciBehindBaseMsg) != 0 {
				t.Fatalf("want checks passed and no behind-base wait; logs: %v", *logs)
			}
		})
	}
}
