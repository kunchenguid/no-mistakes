package steps

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	conflictingBaseLine = "base rewrote this line\n"
	additiveResolution  = "base rewrote this line\nfeature\n"
)

// advanceConflictingBase moves origin/main over the file the published feature
// head added, so the PR genuinely conflicts with its base the way the forge
// reports it.
func (f *ciRepairFixture) advanceConflictingBase(t *testing.T) string {
	t.Helper()
	gitCmd(t, f.dir, "checkout", "main")
	writeFixtureFile(t, f.dir, "feature.txt", conflictingBaseLine)
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "advance base over the same line")
	mainTip := gitCmd(t, f.dir, "rev-parse", "HEAD")
	gitCmd(t, f.dir, "push", "origin", "main")
	gitCmd(t, f.dir, "checkout", "feature")
	return mainTip
}

// withGateMirror gives the fixture a private gate mirror carrying the published
// head, so publication crosses the at-risk mirror reconciliation it meets in
// production.
func (f *ciRepairFixture) withGateMirror(t *testing.T) string {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "gate.git")
	gitCmd(t, f.dir, "init", "--bare", gate)
	gitCmd(t, f.dir, "push", gate, f.headSHA+":refs/heads/feature")
	f.sctx.GateDir = gate
	return gate
}

func (f *ciRepairFixture) useAgent(runFn func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error)) *mockAgent {
	ag := &mockAgent{name: "test", runFn: runFn}
	f.sctx.Agent = ag
	return ag
}

func (f *ciRepairFixture) conflictRepair(t *testing.T, targets ciFixTargets) (ciRepairResult, error) {
	t.Helper()
	host, skip := buildHost(f.sctx, scm.ProviderGitHub)
	if host == nil {
		t.Fatalf("buildHost returned nil: %s", skip)
	}
	pr := &scm.PR{Number: "42", URL: *f.sctx.Run.PRURL}
	return (&CIStep{}).autoFixCI(f.sctx, host, pr, targets)
}

// resolveConflictAsPrompted is a repair agent that does what its prompt asks:
// it concludes a merge the step left stopped on conflicts, or rebases onto the
// base branch when that is what it was told to do.
func resolveConflictAsPrompted(t *testing.T, dir, prompt string) {
	t.Helper()
	switch {
	case mergeInProgress(context.Background(), dir):
		writeFixtureFile(t, dir, "feature.txt", additiveResolution)
		fixtureGit(t, dir, "add", "feature.txt")
		fixtureGit(t, dir, "commit", "--no-edit")
	case strings.Contains(strings.ToLower(prompt), "rebase onto the base branch"):
		fixtureGitAllowFail(t, dir, "rebase", "origin/main")
		writeFixtureFile(t, dir, "feature.txt", additiveResolution)
		fixtureGit(t, dir, "add", "feature.txt")
		fixtureGit(t, dir, "rebase", "--continue")
	default:
		t.Fatalf("the prompt asks for neither a merge nor a rebase:\n%s", prompt)
	}
}

func repairConclusion() *agent.Result {
	return &agent.Result{Output: json.RawMessage(`{"summary":"resolve the conflict with the base","code_change_needed":true}`)}
}

// Under rebase.strategy merge the PR branch is published and must only ever be
// appended to. A conflict repair therefore integrates the base the way the
// Rebase step does - a merge commit whose first parent is the published head -
// and, because that head stays in history, publishes as a fast-forward
// through the gate mirror without a revalidation cycle.
func TestCIStep_MergeStrategyConflictRepairAppendsAMergeCommit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		targets ciFixTargets
	}{
		{name: "merge_conflict_only", targets: ciTargetsFor(nil, true)},
		{name: "failing_check_and_merge_conflict", targets: ciTargetsFor([]string{"test"}, true)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newCIRepairFixture(t, false, nil)
			f.sctx.Config.Rebase.Strategy = config.RebaseStrategyMerge
			gate := f.withGateMirror(t)
			mainTip := f.advanceConflictingBase(t)
			var prompt string
			f.useAgent(func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				prompt = opts.Prompt
				resolveConflictAsPrompted(t, opts.CWD, opts.Prompt)
				return repairConclusion(), nil
			})

			repair, err := f.conflictRepair(t, tc.targets)
			if err != nil {
				t.Fatalf("conflict repair: %v\nlog:\n%s", err, f.log())
			}
			if strings.Contains(strings.ToLower(prompt), "rebase onto the base branch") {
				t.Fatalf("the merge-strategy prompt tells the agent to rebase:\n%s", prompt)
			}
			for _, want := range []string{"Resolve ADDITIVELY", "git commit --no-edit", "feature.txt", "merge target commit: " + mainTip} {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt is missing %q:\n%s", want, prompt)
				}
			}

			head := f.localHead(t)
			if got := parents(t, f.dir, head); len(got) != 2 || got[0] != f.headSHA || got[1] != mainTip {
				t.Fatalf("repaired head %s parents = %v, want the merge [%s %s]", head, got, f.headSHA, mainTip)
			}
			if resolved := gitCmd(t, f.dir, "show", head+":feature.txt"); resolved+"\n" != additiveResolution {
				t.Errorf("resolved feature.txt = %q, want both sides kept", resolved)
			}
			if !repair.HeadAdvanced || repair.Revalidate {
				t.Fatalf("repair = %#v, want a published repair that needs no revalidation", repair)
			}
			if f.remoteHead(t) != head {
				t.Fatalf("remote head = %s, want the merge %s published", f.remoteHead(t), head)
			}
			if gateHead := gitCmd(t, gate, "rev-parse", "refs/heads/feature"); gateHead != head {
				t.Fatalf("gate mirror = %s, want it fast-forwarded to %s", gateHead, head)
			}
			run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != f.headSHA {
				t.Errorf("review approval = %v, want it kept at %s", run.ReviewApprovedHeadSHA, f.headSHA)
			}
			if run.LastPushedSHA == nil || *run.LastPushedSHA != head {
				t.Errorf("push binding = %v, want the merge %s", run.LastPushedSHA, head)
			}
		})
	}
}

// The forge's conflict report can be stale by the time the round runs. When the
// merge concludes on its own and the conflict was the round's only target,
// there is nothing left for a fix agent, so the merge is published without one.
func TestCIStep_MergeStrategyConflictThatMergesCleanlyNeedsNoAgent(t *testing.T) {
	t.Parallel()
	f := newCIRepairFixture(t, false, nil)
	f.sctx.Config.Rebase.Strategy = config.RebaseStrategyMerge
	gitCmd(t, f.dir, "checkout", "main")
	writeFixtureFile(t, f.dir, "main.txt", "main line\n")
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "advance base elsewhere")
	mainTip := gitCmd(t, f.dir, "rev-parse", "HEAD")
	gitCmd(t, f.dir, "push", "origin", "main")
	gitCmd(t, f.dir, "checkout", "feature")
	ag := f.useAgent(func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return repairConclusion(), nil
	})

	repair, err := f.conflictRepair(t, ciTargetsFor(nil, true))
	if err != nil {
		t.Fatalf("conflict repair: %v\nlog:\n%s", err, f.log())
	}
	if len(ag.calls) != 0 {
		t.Fatalf("the fix agent ran %d time(s) with nothing left to resolve", len(ag.calls))
	}
	head := f.localHead(t)
	if got := parents(t, f.dir, head); len(got) != 2 || got[0] != f.headSHA || got[1] != mainTip {
		t.Fatalf("repaired head %s parents = %v, want the merge [%s %s]", head, got, f.headSHA, mainTip)
	}
	if !repair.HeadAdvanced || repair.Revalidate || repair.Summary == "" {
		t.Fatalf("repair = %#v, want a published merge with a summary", repair)
	}
	if f.remoteHead(t) != head {
		t.Fatalf("remote head = %s, want the merge %s published", f.remoteHead(t), head)
	}
}

// A merge-strategy conflict repair that ends the conflict any way other than
// the merge is refused before it can be recorded or pushed, and the worktree is
// put back on the published head so nothing later reads the refused head as
// the branch's state.
func TestCIStep_MergeStrategyRefusesAConflictRepairThatDoesNotMerge(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		rewrite func(t *testing.T, dir string)
	}{
		{
			name: "abandons_the_merge_and_rebases",
			rewrite: func(t *testing.T, dir string) {
				fixtureGitAllowFail(t, dir, "merge", "--abort")
				fixtureGitAllowFail(t, dir, "rebase", "origin/main")
				writeFixtureFile(t, dir, "feature.txt", additiveResolution)
				fixtureGit(t, dir, "add", "feature.txt")
				fixtureGit(t, dir, "rebase", "--continue")
			},
		},
		{
			name: "resets_onto_the_base",
			rewrite: func(t *testing.T, dir string) {
				fixtureGitAllowFail(t, dir, "merge", "--abort")
				fixtureGit(t, dir, "reset", "--hard", "origin/main")
			},
		},
		{
			// Committing this for the agent would publish conflict markers.
			name:    "leaves_the_conflict_unresolved",
			rewrite: func(t *testing.T, dir string) {},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newCIRepairFixture(t, false, nil)
			f.sctx.Config.Rebase.Strategy = config.RebaseStrategyMerge
			f.advanceConflictingBase(t)
			f.useAgent(func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				tc.rewrite(t, opts.CWD)
				return repairConclusion(), nil
			})

			repair, err := f.conflictRepair(t, ciTargetsFor(nil, true))
			if err == nil {
				t.Fatalf("a conflict repair that did not merge the base was accepted: %#v\nlog:\n%s", repair, f.log())
			}
			if repair.HeadAdvanced {
				t.Fatalf("a refused repair was recorded: %#v", repair)
			}
			assertPublishedHeadUntouched(t, f)
		})
	}
}

// The published-head rule belongs to the merge shape, not to the conflict
// prompt: any CI repair that rewrites the published head under merge is
// refused, while the rebase strategy keeps holding it for revalidation.
func TestCIStep_RepairThatAmendsThePublishedHeadFollowsTheStrategy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		strategy    string
		wantRefused bool
	}{
		{strategy: config.RebaseStrategyMerge, wantRefused: true},
		{strategy: config.RebaseStrategyRebase, wantRefused: false},
	} {
		tc := tc
		t.Run(tc.strategy, func(t *testing.T) {
			f := newCIRepairFixture(t, false, nil)
			f.sctx.Config.Rebase.Strategy = tc.strategy
			writeCIFix(f.dir)
			gitCmd(t, f.dir, "add", "-A")
			gitCmd(t, f.dir, "commit", "--amend", "-m", "feature, amended with the fix")
			amended := f.localHead(t)

			repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check", nil)
			if tc.wantRefused {
				if err == nil || !strings.Contains(err.Error(), "published head") {
					t.Fatalf("err = %v, want the rewrite of the published head refused\nlog:\n%s", err, f.log())
				}
				if repair.HeadAdvanced {
					t.Fatalf("a refused repair was recorded: %#v", repair)
				}
				assertPublishedHeadUntouched(t, f)
				return
			}
			if err != nil {
				t.Fatalf("rebase strategy: %v\nlog:\n%s", err, f.log())
			}
			if !repair.HeadAdvanced || !repair.Revalidate {
				t.Fatalf("repair = %#v, want the rewrite held for revalidation", repair)
			}
			if f.sctx.Run.HeadSHA != amended || f.remoteHead(t) != f.headSHA {
				t.Fatalf("recorded=%s remote=%s, want %s recorded locally and %s still published", f.sctx.Run.HeadSHA, f.remoteHead(t), amended, f.headSHA)
			}
		})
	}
}

// A fix agent cut off by its budget is the one CI path that records a head the
// agent left behind without asking anyone. Under merge that head must still
// append to the published head, or it is refused like any other repair.
func TestCIStep_MergeStrategyTimedOutRepairThatRewroteThePublishedHeadIsNotRecorded(t *testing.T) {
	t.Parallel()
	f := newCIRepairFixture(t, false, nil)
	f.sctx.Config.Rebase.Strategy = config.RebaseStrategyMerge
	f.sctx.Config.AgentTimeout = 50 * time.Millisecond
	f.useAgent(func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		writeCIFix(opts.CWD)
		fixtureGit(t, opts.CWD, "add", "-A")
		fixtureGit(t, opts.CWD, "commit", "--amend", "-m", "feature, amended with the fix")
		<-ctx.Done()
		return nil, ctx.Err()
	})

	outcome, err := driveCI(t, &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return nil }}, f.sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, %v; want a parked budget cut\nlog:\n%s", outcome, err, f.log())
	}
	if !strings.Contains(outcome.Findings, "published head") || strings.Contains(outcome.Findings, "recorded locally") {
		t.Fatalf("findings = %s, want the rewrite named as refused and nothing recorded", outcome.Findings)
	}
	assertPublishedHeadUntouched(t, f)
}

func TestCIStep_MergeStrategyRepairWithoutARecordedPublicationIsRefused(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Rebase.Strategy = config.RebaseStrategyMerge
	writeCIFix(dir)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "repair")
	repairHead := gitCmd(t, dir, "rev-parse", "HEAD")

	_, err := (&CIStep{}).recordLocalRepair(sctx, repairHead)
	if err == nil || !strings.Contains(err.Error(), "published head could not be read") {
		t.Fatalf("err = %v, want the repair refused for want of a publication to append to", err)
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("recorded head = %s, want %s kept", sctx.Run.HeadSHA, headSHA)
	}
	assertRestoredToReviewedHead(t, dir, headSHA)
}

func assertPublishedHeadUntouched(t *testing.T, f *ciRepairFixture) {
	t.Helper()
	if f.remoteHead(t) != f.headSHA {
		t.Fatalf("remote head = %s, want the published head %s untouched", f.remoteHead(t), f.headSHA)
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != f.headSHA || f.sctx.Run.HeadSHA != f.headSHA {
		t.Fatalf("recorded head = %s (live %s), want the published head %s", run.HeadSHA, f.sctx.Run.HeadSHA, f.headSHA)
	}
	if run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != f.headSHA {
		t.Fatalf("review approval = %v, want it kept at %s", run.ReviewApprovedHeadSHA, f.headSHA)
	}
	assertRestoredToReviewedHead(t, f.dir, f.headSHA)
	if mergeInProgress(context.Background(), f.dir) || rebaseInProgress(context.Background(), f.dir) {
		t.Fatal("the refused repair left a merge or rebase in progress")
	}
}

func TestCIStep_RebaseStrategyConflictRepairStillRebases(t *testing.T) {
	t.Parallel()
	f := newCIRepairFixture(t, false, nil)
	f.sctx.Config.Rebase.Strategy = config.RebaseStrategyRebase
	f.advanceConflictingBase(t)
	var prompt string
	f.useAgent(func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompt = opts.Prompt
		resolveConflictAsPrompted(t, opts.CWD, opts.Prompt)
		return repairConclusion(), nil
	})

	repair, err := f.conflictRepair(t, ciTargetsFor(nil, true))
	if err != nil {
		t.Fatalf("conflict repair: %v\nlog:\n%s", err, f.log())
	}
	if !strings.Contains(prompt, "Rebase onto the base branch and resolve the merge conflicts") || strings.Contains(prompt, "Resolve ADDITIVELY") {
		t.Fatalf("the rebase-strategy prompt changed:\n%s", prompt)
	}
	if !repair.HeadAdvanced || !repair.Revalidate {
		t.Fatalf("repair = %#v, want the rebased head held for revalidation", repair)
	}
	if got := parents(t, f.dir, f.localHead(t)); len(got) != 1 {
		t.Fatalf("rebase-strategy repair has parents %v, want a linear rebase", got)
	}
}

func commitAsForge(t *testing.T, dir, message string) {
	t.Helper()
	cmd := exec.Command("git", "commit", "-m", message)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Another Contributor", "GIT_AUTHOR_EMAIL=another@example.com",
		"GIT_COMMITTER_NAME=GitHub", "GIT_COMMITTER_EMAIL=noreply@github.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
}

func TestCIStep_MergeStrategyCleanMergeOverForgeMergedBaseIsNotRefused(t *testing.T) {
	t.Parallel()
	f := newCIRepairFixture(t, false, nil)
	f.sctx.Config.Rebase.Strategy = config.RebaseStrategyMerge
	gitCmd(t, f.dir, "checkout", "main")
	writeFixtureFile(t, f.dir, "main.txt", "main line\n")
	gitCmd(t, f.dir, "add", "-A")
	commitAsForge(t, f.dir, "squash-merged elsewhere (#7)")
	gitCmd(t, f.dir, "push", "origin", "main")
	gitCmd(t, f.dir, "checkout", "feature")
	f.useAgent(func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return repairConclusion(), nil
	})
	repair, err := f.conflictRepair(t, ciTargetsFor(nil, true))
	if err != nil {
		t.Fatalf("clean merge repair refused: %v\nlog:\n%s", err, f.log())
	}
	if !repair.HeadAdvanced {
		t.Fatalf("repair = %#v, want the merge published", repair)
	}
}
