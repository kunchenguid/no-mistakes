package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// mergeFixture builds a repository whose feature branch and origin/main have
// both moved since they diverged, which is the only situation the rebase step
// actually integrates anything in. sharedContent decides whether they collide:
// give main and feature different content for the same file to force a
// conflict, or touch separate files for a clean integration.
type mergeFixture struct {
	dir      string
	upstream string
	baseSHA  string
	headSHA  string // the feature head the pipeline reviewed, pre-integration
	mainSHA  string // origin/main after it moved
}

func newMergeFixture(t *testing.T, conflicting bool) mergeFixture {
	t.Helper()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	writeFixtureFile(t, dir, "shared.txt", "base\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	if conflicting {
		writeFixtureFile(t, dir, "shared.txt", "feature line\n")
	} else {
		writeFixtureFile(t, dir, "feature.txt", "feature line\n")
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature change")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "main")
	if conflicting {
		writeFixtureFile(t, dir, "shared.txt", "main line\n")
	} else {
		writeFixtureFile(t, dir, "main.txt", "main line\n")
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main advance")
	mainSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	return mergeFixture{dir: dir, upstream: upstream, baseSHA: baseSHA, headSHA: headSHA, mainSHA: mainSHA}
}

func writeFixtureFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f mergeFixture) context(t *testing.T, ag agent.Agent, strategy string) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContextWithDBRecords(t, ag, f.dir, f.baseSHA, f.headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = f.upstream
	if strategy != "" {
		sctx.Config.Rebase.Strategy = strategy
	}
	return sctx
}

// parents returns a commit's parent SHAs in order. The FIRST one is what the
// whole merge strategy turns on: it must stay the head the pipeline reviewed.
func parents(t *testing.T, dir, rev string) []string {
	t.Helper()
	out := gitCmd(t, dir, "rev-list", "--parents", "-n", "1", rev)
	fields := strings.Fields(out)
	if len(fields) < 1 {
		t.Fatalf("rev-list --parents returned %q", out)
	}
	return fields[1:]
}

// assertRestoredToReviewedHead is what every rejected merge shape owes the run:
// the step fails, and the worktree is left back on the head the pipeline
// reviewed with nothing of the rejected attempt staged, modified, or untracked
// on top of it. Anything else leaves the invalid head checked out as if it were
// the branch's real state.
func assertRestoredToReviewedHead(t *testing.T, dir, reviewedHead string) {
	t.Helper()
	if head := gitCmd(t, dir, "rev-parse", "HEAD"); head != reviewedHead {
		t.Fatalf("head after the rejected merge = %s, want the reviewed head %s restored", head, reviewedHead)
	}
	if status := gitCmd(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("worktree after the rejected merge is not clean:\n%s", status)
	}
}

func TestRebaseStep_MergeStrategyIntegratesMovedBaseAsAMergeCommit(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, false)
	sctx := f.context(t, &mockAgent{name: "test"}, config.RebaseStrategyMerge)

	outcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("expected a clean merge, got approval gate: %s", outcome.Findings)
	}

	head := gitCmd(t, f.dir, "rev-parse", "HEAD")
	got := parents(t, f.dir, head)
	if len(got) != 2 {
		t.Fatalf("head %s has %d parent(s) %v, want a merge commit with 2", head, len(got), got)
	}
	if got[0] != f.headSHA {
		t.Fatalf("first parent = %s, want the reviewed head %s", got[0], f.headSHA)
	}
	if got[1] != f.mainSHA {
		t.Fatalf("second parent = %s, want origin/main %s", got[1], f.mainSHA)
	}
	// Both sides' work is present in the tree.
	for _, name := range []string{"feature.txt", "main.txt"} {
		if _, err := os.Stat(filepath.Join(f.dir, name)); err != nil {
			t.Fatalf("expected %s after the merge: %v", name, err)
		}
	}
	if status := gitStatusPorcelain(t, f.dir); status != "" {
		t.Fatalf("expected a clean worktree, got: %s", status)
	}
	if sctx.Run.HeadSHA != head {
		t.Fatalf("run head = %s, want the merge commit %s", sctx.Run.HeadSHA, head)
	}
}

func TestRebaseStep_MergeStrategyResolvesConflictAdditively(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, true)

	var prompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			prompt = opts.Prompt
			// An additive resolution: keep what BOTH sides introduced.
			writeFixtureFile(t, f.dir, "shared.txt", "main line\nfeature line\n")
			fixtureGit(t, f.dir, "add", "shared.txt")
			fixtureGit(t, f.dir, "commit", "--no-edit")
			return &agent.Result{Output: json.RawMessage(`{"summary":"kept both sides"}`)}, nil
		},
	}

	sctx := f.context(t, ag, config.RebaseStrategyMerge)
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 agent call, got %d", len(ag.calls))
	}
	for _, want := range []string{
		"Resolve ADDITIVELY",
		"never delete content one side introduced",
		"git commit --no-edit",
		"shared.txt",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("resolver prompt is missing %q, got:\n%s", want, prompt)
		}
	}

	head := gitCmd(t, f.dir, "rev-parse", "HEAD")
	got := parents(t, f.dir, head)
	if len(got) != 2 || got[0] != f.headSHA || got[1] != f.mainSHA {
		t.Fatalf("resolved head %s parents = %v, want [%s %s]", head, got, f.headSHA, f.mainSHA)
	}
	resolved := gitCmd(t, f.dir, "show", "HEAD:shared.txt")
	if !strings.Contains(resolved, "feature line") || !strings.Contains(resolved, "main line") {
		t.Fatalf("resolution dropped a side: %q", resolved)
	}
	if mergeInProgress(context.Background(), f.dir) {
		t.Fatal("merge left in progress after the resolution")
	}
}

// An agent that resolves the files but never concludes the merge leaves
// MERGE_HEAD set and the index conflicted. Carrying that forward would publish
// the reviewed head as if the integration had happened, so the step aborts the
// merge and fails instead.
func TestRebaseStep_MergeStrategyUnconcludedMergeIsAbortedAndFails(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, true)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			writeFixtureFile(t, f.dir, "shared.txt", "main line\nfeature line\n")
			fixtureGit(t, f.dir, "add", "shared.txt")
			// Deliberately no commit: the merge is left in progress.
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved"}`)}, nil
		},
	}

	sctx := f.context(t, ag, config.RebaseStrategyMerge)
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err == nil {
		t.Fatal("expected an error for an unconcluded merge, got nil")
	} else if !strings.Contains(err.Error(), "did not complete the merge") {
		t.Fatalf("error = %v, want it to name the unconcluded merge", err)
	}
	if mergeInProgress(context.Background(), f.dir) {
		t.Fatal("merge left in progress; it should have been aborted")
	}
	if head := gitCmd(t, f.dir, "rev-parse", "HEAD"); head != f.headSHA {
		t.Fatalf("head = %s, want the reviewed head %s after the abort", head, f.headSHA)
	}
}

// The reviewed head staying an ancestor is the point of the merge shape: the CI
// step's continuity rule then holds by ancestry, with no patch-id or
// content-based guess, so a later repair can be published rather than paying a
// full revalidation cycle.
func TestRebaseStep_MergeStrategyKeepsReviewedHeadAncestorForCIRepairContinuity(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, false)
	sctx := f.context(t, &mockAgent{name: "test"}, config.RebaseStrategyMerge)

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	head := gitCmd(t, f.dir, "rev-parse", "HEAD")

	// Review approved the pre-integration head.
	recordReviewApproval(t, sctx, f.headSHA)
	if gap := ciRepairContinuityGap(sctx, head); gap != "" {
		t.Fatalf("merged head does not continue the reviewed head: %s", gap)
	}

	// The same fixture rebased instead leaves a head the rule cannot accept.
	fr := newMergeFixture(t, false)
	rsctx := fr.context(t, &mockAgent{name: "test"}, config.RebaseStrategyRebase)
	if _, err := (&RebaseStep{}).Execute(rsctx); err != nil {
		t.Fatal(err)
	}
	recordReviewApproval(t, rsctx, fr.headSHA)
	rebased := gitCmd(t, fr.dir, "rev-parse", "HEAD")
	if gap := ciRepairContinuityGap(rsctx, rebased); gap == "" {
		t.Fatal("expected a rebased head to fail the continuity rule; the merge test proves nothing otherwise")
	}
}

// Publication after a merge must append, never rewrite: an open PR's head and a
// review attestation bound to an exact SHA both depend on it.
func TestRebaseStep_MergeStrategyPublishesAsAFastForwardWithoutForce(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, false)
	// The reviewed head is already published, as it is by the time anything
	// integrates a moved base mid-run.
	gitCmd(t, f.dir, "push", "origin", "feature")

	sctx := f.context(t, &mockAgent{name: "test"}, config.RebaseStrategyMerge)
	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	head := gitCmd(t, f.dir, "rev-parse", "HEAD")

	ctx := context.Background()
	gitRun := func(args ...string) (string, error) { return git.Run(ctx, f.dir, args...) }
	decision, err := resolveForcePushDecision(gitRun, f.upstream, "refs/heads/feature", head, f.headSHA, f.baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.fastForward {
		t.Fatalf("expected a fast-forward publication, got %#v", decision)
	}
	if decision.remoteSHA != f.headSHA {
		t.Fatalf("remote head = %s, want the reviewed head %s", decision.remoteSHA, f.headSHA)
	}
}

// The flag off must leave the step exactly as it was, so an existing
// installation sees no change from a version bump. Unset and an explicit
// "rebase" must also mean the same thing.
func TestRebaseStep_DefaultStrategyStillRebasesUnchanged(t *testing.T) {
	t.Parallel()
	shape := func(strategy string) (tree string, subjects string, parentCount int, reviewedHeadKept bool) {
		f := newMergeFixture(t, false)
		sctx := f.context(t, &mockAgent{name: "test"}, strategy)
		if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
			t.Fatal(err)
		}
		head := gitCmd(t, f.dir, "rev-parse", "HEAD")
		return gitCmd(t, f.dir, "rev-parse", "HEAD^{tree}"),
			gitCmd(t, f.dir, "log", "--format=%s", f.baseSHA+"..HEAD"),
			len(parents(t, f.dir, head)),
			isAncestor(context.Background(), f.dir, f.headSHA, head)
	}

	unsetTree, unsetSubjects, unsetParents, unsetKept := shape("")
	explicitTree, explicitSubjects, explicitParents, explicitKept := shape(config.RebaseStrategyRebase)

	if unsetTree != explicitTree || unsetSubjects != explicitSubjects {
		t.Fatalf("explicit %q differs from unset: trees %s/%s, subjects %q/%q",
			config.RebaseStrategyRebase, unsetTree, explicitTree, unsetSubjects, explicitSubjects)
	}
	if unsetParents != 1 || explicitParents != 1 {
		t.Fatalf("rebase produced a merge commit: parents %d/%d", unsetParents, explicitParents)
	}
	if unsetKept || explicitKept {
		t.Fatal("rebase left the pre-rebase head as an ancestor; the fixture no longer rebases anything")
	}
}

func fixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if err := runFixtureGit(dir, args...); err != nil {
		t.Fatal(err)
	}
}

// runFixtureGit owns the fixture git environment for both callers and returns
// the failure rather than deciding what it means.
func runFixtureGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
		"GIT_EDITOR=true",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %v: %s: %w", args, out, err)
	}
	return nil
}

// An agent can also end the conflict by abandoning it: `git merge --abort`
// clears MERGE_HEAD and restores the reviewed head, so the unconcluded-merge
// guard above sees a clean worktree and passes. Nothing was integrated, so the
// step must still fail rather than carry the un-integrated head forward as if
// the base had been merged in.
func TestRebaseStep_MergeStrategyAbortedMergeFails(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, true)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixtureGit(t, f.dir, "merge", "--abort")
			return &agent.Result{Output: json.RawMessage(`{"summary":"gave up"}`)}, nil
		},
	}

	sctx := f.context(t, ag, config.RebaseStrategyMerge)
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err == nil {
		t.Fatal("expected an error when the agent aborted the merge, got nil")
	} else if !strings.Contains(err.Error(), "did not merge") {
		t.Fatalf("error = %v, want it to name the un-integrated target", err)
	}
	if mergeInProgress(context.Background(), f.dir) {
		t.Fatal("merge left in progress after the abort")
	}
	assertRestoredToReviewedHead(t, f.dir, f.headSHA)
}

// Ending the conflict by rebasing onto the target instead satisfies ancestry -
// the target is in HEAD - while producing exactly the linear history merge mode
// exists to avoid: the reviewed head is gone from the branch, so CI repair
// continuity falls back to a guess and publication rewrites the open PR's head.
// The step must fail rather than log a merge that never happened.
func TestRebaseStep_MergeStrategyRebasedInsteadOfMergedFails(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, true)

	// The head the agent actually left is captured inside the turn, because the
	// step restores the worktree off it before returning; the fixture-integrity
	// assertions still have to be made against that head, not the restored one.
	var rebasedHead string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixtureGit(t, f.dir, "merge", "--abort")
			fixtureGit(t, f.dir, "rebase", "--strategy-option=theirs", "origin/main")
			rebasedHead = gitCmd(t, f.dir, "rev-parse", "HEAD")
			return &agent.Result{Output: json.RawMessage(`{"summary":"rebased"}`)}, nil
		},
	}

	sctx := f.context(t, ag, config.RebaseStrategyMerge)
	sctx.Fixing = true

	_, err := (&RebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected an error when the agent rebased instead of merging, got nil")
	}
	if !strings.Contains(err.Error(), "did not merge") {
		t.Fatalf("error = %v, want it to name the missing merge", err)
	}

	if got := parents(t, f.dir, rebasedHead); len(got) != 1 {
		t.Fatalf("fixture no longer produces a rebase-shaped head: parents %v", got)
	}
	if !isAncestor(context.Background(), f.dir, f.mainSHA, rebasedHead) {
		t.Fatal("fixture no longer satisfies plain ancestry; the test proves nothing")
	}
	assertRestoredToReviewedHead(t, f.dir, f.headSHA)
}

// Nothing forbids the agent from committing again after concluding the merge -
// a textual resolution routinely leaves a semantic one behind. The merge shape
// is intact at that head, so the step must accept it.
func TestRebaseStep_MergeStrategyFollowUpCommitAfterTheMergeIsAccepted(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, true)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			writeFixtureFile(t, f.dir, "shared.txt", "main line\nfeature line\n")
			fixtureGit(t, f.dir, "add", "shared.txt")
			fixtureGit(t, f.dir, "commit", "--no-edit")
			writeFixtureFile(t, f.dir, "shared.txt", "main line\nfeature line\nreconciled\n")
			fixtureGit(t, f.dir, "add", "shared.txt")
			fixtureGit(t, f.dir, "commit", "-m", "fix up the merged tree")
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolved"}`)}, nil
		},
	}

	sctx := f.context(t, ag, config.RebaseStrategyMerge)
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatalf("merge with a follow-up commit was rejected: %v", err)
	}

	head := gitCmd(t, f.dir, "rev-parse", "HEAD")
	mergeCommit := gitCmd(t, f.dir, "rev-parse", "HEAD~1")
	if got := parents(t, f.dir, mergeCommit); len(got) != 2 || got[0] != f.headSHA || got[1] != f.mainSHA {
		t.Fatalf("merge commit %s parents = %v, want [%s %s]", mergeCommit, got, f.headSHA, f.mainSHA)
	}
	if !isAncestor(context.Background(), f.dir, f.headSHA, head) {
		t.Fatalf("reviewed head %s is not in %s", f.headSHA, head)
	}
}

// An agent can also abandon the merge and then commit something else entirely.
// That keeps the reviewed head in the history and does move HEAD, so both the
// moved-head and reviewed-head-ancestor conditions pass; only the target's own
// ancestry catches it. Without that check the run would carry on having never
// integrated the moved base.
func TestRebaseStep_MergeStrategyUnrelatedCommitInsteadOfMergeFails(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, true)

	var unrelatedHead string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixtureGit(t, f.dir, "merge", "--abort")
			writeFixtureFile(t, f.dir, "unrelated.txt", "not the merge\n")
			fixtureGit(t, f.dir, "add", "unrelated.txt")
			fixtureGit(t, f.dir, "commit", "-m", "unrelated work")
			unrelatedHead = gitCmd(t, f.dir, "rev-parse", "HEAD")
			return &agent.Result{Output: json.RawMessage(`{"summary":"committed something else"}`)}, nil
		},
	}

	sctx := f.context(t, ag, config.RebaseStrategyMerge)
	sctx.Fixing = true

	_, err := (&RebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected an error when the agent committed instead of merging, got nil")
	}
	if !strings.Contains(err.Error(), "did not merge") {
		t.Fatalf("error = %v, want it to name the missing merge", err)
	}

	// Fixture integrity: the head the agent left must be one ONLY the
	// target-ancestry check can reject, or the test proves nothing about that
	// check. It is read from the capture, not from HEAD, because the step has
	// since restored the worktree off it.
	if unrelatedHead == f.headSHA {
		t.Fatal("fixture left HEAD unmoved; the moved-head check would reject this instead")
	}
	if !isAncestor(context.Background(), f.dir, f.headSHA, unrelatedHead) {
		t.Fatal("fixture dropped the reviewed head; the reviewed-head check would reject this instead")
	}
	if isAncestor(context.Background(), f.dir, f.mainSHA, unrelatedHead) {
		t.Fatal("fixture integrated the target after all; the test proves nothing")
	}
	assertRestoredToReviewedHead(t, f.dir, f.headSHA)
}

// Resetting hard onto the target is the third way to end the conflict without
// merging: the target is in HEAD and HEAD has moved, so only the reviewed-head
// ancestry check rejects it. It is also the shape that loses the most if the
// invalid head is retained - the reviewed commits are not in it at all.
func TestRebaseStep_MergeStrategyResetOntoTargetFails(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, true)

	var resetHead string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixtureGit(t, f.dir, "merge", "--abort")
			fixtureGit(t, f.dir, "reset", "--hard", "origin/main")
			resetHead = gitCmd(t, f.dir, "rev-parse", "HEAD")
			return &agent.Result{Output: json.RawMessage(`{"summary":"took theirs"}`)}, nil
		},
	}

	sctx := f.context(t, ag, config.RebaseStrategyMerge)
	sctx.Fixing = true

	_, err := (&RebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected an error when the agent reset onto the target instead of merging, got nil")
	}
	if !strings.Contains(err.Error(), "did not merge") {
		t.Fatalf("error = %v, want it to name the missing merge", err)
	}

	if resetHead != f.mainSHA {
		t.Fatalf("fixture head = %s, want the target %s; the reset did not happen", resetHead, f.mainSHA)
	}
	if isAncestor(context.Background(), f.dir, f.headSHA, resetHead) {
		t.Fatal("fixture kept the reviewed head; the test proves nothing about the reviewed-head check")
	}
	assertRestoredToReviewedHead(t, f.dir, f.headSHA)
}

// fixtureGitAllowFail runs a fixture git command that is EXPECTED to exit
// non-zero, which a conflicted rebase does.
func fixtureGitAllowFail(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = runFixtureGit(dir, args...)
}

// Abandoning the merge and rebasing onto the same target hits the same
// conflict, so the rebase stops with rebase state in place. Git sets no
// MERGE_HEAD for that, so the unconcluded-merge guard passes, and a reset there
// moves HEAD while the interrupted rebase survives underneath it - a restore
// reported as successful on a worktree still mid-rebase. The step must abort
// the rebase and report the un-integrated target with the worktree genuinely
// restored.
func TestRebaseStep_MergeStrategyConflictedRebaseLeftInProgressIsAbortedAndFails(t *testing.T) {
	t.Parallel()
	f := newMergeFixture(t, true)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixtureGit(t, f.dir, "merge", "--abort")
			fixtureGitAllowFail(t, f.dir, "rebase", "origin/main")
			if !rebaseInProgress(ctx, f.dir) {
				t.Fatal("fixture no longer leaves a conflicted rebase in progress; the test proves nothing")
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"rebased instead"}`)}, nil
		},
	}

	sctx := f.context(t, ag, config.RebaseStrategyMerge)
	sctx.Fixing = true

	_, err := (&RebaseStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected an error when the agent left a conflicted rebase in progress, got nil")
	}
	if !strings.Contains(err.Error(), "did not merge") {
		t.Fatalf("error = %v, want it to name the missing merge", err)
	}

	if rebaseInProgress(context.Background(), f.dir) {
		t.Fatal("rebase left in progress after the rejection")
	}
	if ref := gitCmd(t, f.dir, "rev-parse", "refs/heads/feature"); ref != f.headSHA {
		t.Fatalf("branch ref after the rejection = %s, want the reviewed head %s", ref, f.headSHA)
	}
	assertRestoredToReviewedHead(t, f.dir, f.headSHA)
}
