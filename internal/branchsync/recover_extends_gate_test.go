package branchsync

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// strandedTestFixOptions shape the issue #1421 fixture's deliberate deviations.
type strandedTestFixOptions struct {
	// withoutReviewAuthority leaves review_approved_head_sha NULL, as a run that
	// never completed a review would.
	withoutReviewAuthority bool
	// rebased replays the submitted commit onto the base before the fix commits,
	// so the recorded head no longer descends from the submitted head.
	rebased bool
}

// newStrandedTestFixFixture reproduces issue #1421 exactly as the run left it.
// The pipeline ran in a detached worktree of the gate (git.WorktreeAdd), so its
// Review fix and Test fix commits entered the gate's object store while the gate
// branch stayed at the submitted head. Review approved the Review fix, Test
// moved the head again, and Document then failed on a transient fetch - a
// non-code failure - before Push. The worktree was removed when the run
// finished, leaving the recorded head as an unreferenced gate object, the
// terminal head never verified, and the operator's clean checkout sitting
// exactly at that recorded head (the reporter's state when every documented
// next action refused). It returns the fixture and the review-approved head.
func newStrandedTestFixFixture(t *testing.T, opts strandedTestFixOptions) (*recoverFixture, string) {
	t.Helper()
	f := newRecoverFixtureAtSubmittedHead(t)

	runWorktree := filepath.Join(filepath.Dir(f.local), "run-worktree")
	start := f.submitted
	if opts.rebased {
		// The default branch advanced after submission, so the pipeline's
		// rebase replays the submitted commit as a new SHA.
		mustRun(t, f.local, "checkout", "main")
		mustWrite(t, filepath.Join(f.local, "upstream.txt"), "upstream advance\n")
		mustRun(t, f.local, "add", "upstream.txt")
		mustRun(t, f.local, "commit", "-m", "upstream advance")
		start = mustRun(t, f.local, "rev-parse", "HEAD")
		mustRun(t, f.local, "push", f.gate, "refs/heads/main:refs/heads/main")
		mustRun(t, f.local, "checkout", "feature/recover")
	}
	mustRun(t, f.gate, "worktree", "add", "--detach", runWorktree, start)
	if opts.rebased {
		mustRun(t, runWorktree, "cherry-pick", f.submitted)
	}
	mustWrite(t, filepath.Join(runWorktree, "review-fix.txt"), "review fix\n")
	mustRun(t, runWorktree, "add", "review-fix.txt")
	mustRun(t, runWorktree, "commit", "-m", "no-mistakes(review): fix")
	reviewed := mustRun(t, runWorktree, "rev-parse", "HEAD")
	mustWrite(t, filepath.Join(runWorktree, "test-fix.txt"), "test fix\n")
	mustRun(t, runWorktree, "add", "test-fix.txt")
	mustRun(t, runWorktree, "commit", "-m", "no-mistakes(test): fix")
	recorded := mustRun(t, runWorktree, "rev-parse", "HEAD")
	mustRun(t, f.gate, "worktree", "remove", runWorktree)
	if !objectExists(f.ctx, f.gate, recorded) {
		t.Fatal("fixture lost the recorded head when the run worktree was removed")
	}

	if err := f.db.UpdateRunHeadSHA(f.run.ID, recorded); err != nil {
		t.Fatal(err)
	}
	if !opts.withoutReviewAuthority {
		if err := f.db.UpdateRunReviewApprovedHeadSHA(f.run.ID, reviewed); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.db.UpdateRunErrorStatus(f.run.ID, `step document failed: fetch default branch "main" to resolve branch base: git fetch --no-tags origin +refs/heads/main:refs/remotes/origin/main: exit status 128: fatal: unable to access 'https://example.com/': SSL connection timeout`, types.RunFailed); err != nil {
		t.Fatal(err)
	}

	// The operator checked the exact preserved head out, as the reporter did,
	// through a temporary ref because the object is otherwise unreachable.
	mustRun(t, f.gate, "update-ref", "refs/no-mistakes/test-stranded", recorded)
	mustRun(t, f.local, "fetch", "--no-tags", f.gate, "refs/no-mistakes/test-stranded")
	mustRun(t, f.gate, "update-ref", "-d", "refs/no-mistakes/test-stranded")
	mustRun(t, f.local, "reset", "--hard", recorded)

	f.preserved = recorded
	f.run, _ = f.db.GetRun(f.run.ID)
	return f, reviewed
}

// newRecoverFixtureAtSubmittedHead builds the operator repo, the upstream, and
// a gate whose feature/recover branch holds exactly the submitted head, plus
// the run row for that submission; the pipeline's work is added by the caller.
func newRecoverFixtureAtSubmittedHead(t *testing.T) *recoverFixture {
	t.Helper()
	root, heads := copyGitFixture(t, "recover-at-submitted-head", buildRecoverRepositoriesAtSubmittedHead)
	remote := filepath.Join(root, "upstream.git")
	local := filepath.Join(root, "operator")
	gate := filepath.Join(root, "gate.git")
	base, submitted := heads.base, heads.submitted

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(local, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/recover", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	return &recoverFixture{
		t: t, ctx: t.Context(), db: database, repo: repo, run: run,
		service: &Service{DB: database, Repo: repo, WorkDir: local, GateDir: gate},
		local:   local, gate: gate, remote: remote,
		base: base, submitted: submitted,
	}
}

func buildRecoverRepositoriesAtSubmittedHead(t *testing.T, root string) recoverHeads {
	t.Helper()
	remote := filepath.Join(root, "upstream.git")
	mustRun(t, root, "init", "--bare", remote)

	local := filepath.Join(root, "operator")
	mustRun(t, root, "init", "-b", "main", local)
	mustWrite(t, filepath.Join(local, "file.txt"), "base\n")
	mustRun(t, local, "add", "file.txt")
	mustRun(t, local, "commit", "-m", "base")
	base := mustRun(t, local, "rev-parse", "HEAD")
	mustRun(t, local, "checkout", "-b", "feature/recover")
	mustWrite(t, filepath.Join(local, "feature.txt"), "feature\n")
	mustRun(t, local, "add", "feature.txt")
	mustRun(t, local, "commit", "-m", "feature")
	submitted := mustRun(t, local, "rev-parse", "HEAD")

	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gate)
	mustRun(t, local, "push", gate, "refs/heads/main:refs/heads/main", "refs/heads/feature/recover:refs/heads/feature/recover")
	return recoverHeads{base: base, submitted: submitted}
}

// commitOutsidePreservedHistory adds one commit on top of the submitted head in
// the gate that the recorded head does not contain, and returns it.
func commitOutsidePreservedHistory(t *testing.T, f *recoverFixture) string {
	t.Helper()
	side := filepath.Join(filepath.Dir(f.local), "side-worktree")
	mustRun(t, f.gate, "worktree", "add", "--detach", side, f.submitted)
	mustWrite(t, filepath.Join(side, "side.txt"), "outside the preserved history\n")
	mustRun(t, side, "add", "side.txt")
	mustRun(t, side, "commit", "-m", "outside the preserved history")
	sha := mustRun(t, side, "rev-parse", "HEAD")
	mustRun(t, f.gate, "update-ref", "refs/no-mistakes/test-side", sha)
	mustRun(t, f.gate, "worktree", "remove", side)
	return sha
}

// TestRecoverReturnsCustodyAtTheRecordedHeadThatExtendsTheGateBranch is the
// regression test for issue #1421. Before the fix, Recover reached the
// equal-tree rewrite proof for this shape, found two different trees, and
// refused with blocked_recover_unverified_head - while axi status kept
// pointing at `sync --recover` as the next action.
func TestRecoverReturnsCustodyAtTheRecordedHeadThatExtendsTheGateBranch(t *testing.T) {
	t.Parallel()

	f, reviewed := newStrandedTestFixFixture(t, strandedTestFixOptions{})
	gateHead := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover")
	if f.run.TerminalHeadVerifiedAt != nil {
		t.Fatal("fixture terminal head is already verified")
	}
	if gateHead != f.submitted {
		t.Fatalf("fixture gate branch = %s, want the submitted head %s", gateHead, f.submitted)
	}
	if isAncestor(f.ctx, f.gate, f.preserved, gateHead) || !isAncestor(f.ctx, f.gate, gateHead, f.preserved) {
		t.Fatal("fixture recorded head does not strictly extend the gate branch")
	}
	if got, want := mustRun(t, f.gate, "rev-parse", f.preserved+"^{tree}"), mustRun(t, f.gate, "rev-parse", gateHead+"^{tree}"); got == want {
		t.Fatal("fixture trees are equal, so the pre-fix rewrite proof would already have accepted it")
	}
	if got := mustRun(t, f.gate, "for-each-ref", "--contains", f.preserved); got != "" {
		t.Fatalf("fixture recorded head is still referenced in the gate: %q", got)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("fixture operator HEAD = %s, want the recorded head %s", got, f.preserved)
	}
	before, _, _ := f.service.inspect(f.ctx)
	if before.Safety != "blocked_pipeline_owned_recoverable" || before.NextAction == nil || before.NextAction.Command != "no-mistakes axi sync --recover" {
		t.Fatalf("pre-recovery status = %s / %#v, want the recover_custody offer this test makes honest", before.Safety, before.NextAction)
	}
	runsBefore := len(mustRuns(t, f.db, f.repo.ID))
	remoteBefore := mustRun(t, f.remote, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads")

	state := f.service.Recover(f.ctx, false)
	if !state.Recovered || state.Changed || state.State != StateCustodyReturned || state.Safety != "custody_returned" {
		t.Fatalf("recovery at the recorded head = %#v", state)
	}
	if state.NextAction == nil || state.NextAction.Code != "run_pipeline" {
		t.Fatalf("post-recovery next action = %#v, want run_pipeline", state.NextAction)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("HEAD = %s, want the untouched recorded head %s", got, f.preserved)
	}
	if clean, reason := worktreeClean(f.ctx, f.local); !clean {
		t.Fatalf("worktree not clean after recovery: %s", reason)
	}
	if got := mustRun(t, f.gate, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("gate recovery anchor = %s, want %s", got, f.preserved)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("local recovery anchor = %s, want %s", got, f.preserved)
	}
	if _, err := gitpkg.Run(f.ctx, f.local, "rev-parse", "--verify", f.localAnchorRef()+"^{commit}"); err == nil {
		t.Fatal("recovery anchored a pre-recovery head although the worktree never moved")
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
		t.Fatalf("recovery moved the gate branch to %s", got)
	}
	run, err := f.db.GetRun(f.run.ID)
	if err != nil || run.HeadSHA != f.preserved || run.TerminalHeadVerifiedAt == nil || run.CustodyReturnedAt == nil || ptr(run.ReviewApprovedHeadSHA) != reviewed || run.Status != types.RunFailed {
		t.Fatalf("recovered run = %#v, err %v", run, err)
	}
	if second := f.service.Recover(f.ctx, false); !second.Recovered || second.Changed {
		t.Fatalf("repeated recovery = %#v", second)
	}
	if got := len(mustRuns(t, f.db, f.repo.ID)); got != runsBefore {
		t.Fatalf("recovery changed run count from %d to %d", runsBefore, got)
	}
	if got := mustRun(t, f.remote, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads"); got != remoteBefore {
		t.Fatalf("recovery pushed upstream refs: %q != %q", got, remoteBefore)
	}
}

// TestRecoverRecordedHeadExtendingTheGateBranchFailsClosed pins every edge of
// the #1421 proof: each control removes exactly one premise and recovery must
// go back to refusing without touching a file, a ref, or the run.
func TestRecoverRecordedHeadExtendingTheGateBranchFailsClosed(t *testing.T) {
	t.Parallel()

	t.Run("worktree behind at the submitted head", func(t *testing.T) {
		t.Parallel()
		f, _ := newStrandedTestFixFixture(t, strandedTestFixOptions{})
		mustRun(t, f.local, "reset", "--hard", f.submitted)
		if state := assertUnverifiedRecoveryRefusalNoMutation(t, f); state.Safety != "blocked_recover_unverified_head" {
			t.Fatalf("behind-worktree refusal = %s", state.Safety)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
			t.Fatalf("refusal moved HEAD to %s", got)
		}
	})
	t.Run("dirty worktree", func(t *testing.T) {
		t.Parallel()
		f, _ := newStrandedTestFixFixture(t, strandedTestFixOptions{})
		mustWrite(t, filepath.Join(f.local, "feature.txt"), "dirty local edit\n")
		if state := assertUnverifiedRecoveryRefusalNoMutation(t, f); state.Safety != "blocked_recover_unverified_head" {
			t.Fatalf("dirty-worktree refusal = %s", state.Safety)
		}
		if got := readOptional(t, filepath.Join(f.local, "feature.txt")); got != "dirty local edit\n" {
			t.Fatalf("dirty worktree changed: %q", got)
		}
	})
	t.Run("no review authority", func(t *testing.T) {
		t.Parallel()
		f, _ := newStrandedTestFixFixture(t, strandedTestFixOptions{withoutReviewAuthority: true})
		if f.run.ReviewApprovedHeadSHA != nil {
			t.Fatal("fixture recorded a review authority")
		}
		if state := assertUnverifiedRecoveryRefusalNoMutation(t, f); state.Safety != "blocked_recover_unverified_head" {
			t.Fatalf("no-review refusal = %s", state.Safety)
		}
	})
	t.Run("review authority outside the preserved history", func(t *testing.T) {
		t.Parallel()
		f, _ := newStrandedTestFixFixture(t, strandedTestFixOptions{})
		outside := commitOutsidePreservedHistory(t, f)
		if err := f.db.UpdateRunReviewApprovedHeadSHA(f.run.ID, outside); err != nil {
			t.Fatal(err)
		}
		if state := assertUnverifiedRecoveryRefusalNoMutation(t, f); state.Safety != "blocked_recover_unverified_head" {
			t.Fatalf("foreign-review refusal = %s", state.Safety)
		}
	})
	t.Run("review authority before the submitted head", func(t *testing.T) {
		t.Parallel()
		f, _ := newStrandedTestFixFixture(t, strandedTestFixOptions{})
		if err := f.db.UpdateRunReviewApprovedHeadSHA(f.run.ID, f.base); err != nil {
			t.Fatal(err)
		}
		if state := assertUnverifiedRecoveryRefusalNoMutation(t, f); state.Safety != "blocked_recover_unverified_head" {
			t.Fatalf("pre-submission-review refusal = %s", state.Safety)
		}
	})
	t.Run("gate branch moved outside the preserved history", func(t *testing.T) {
		t.Parallel()
		f, _ := newStrandedTestFixFixture(t, strandedTestFixOptions{})
		outside := commitOutsidePreservedHistory(t, f)
		mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", outside)
		if state := assertUnverifiedRecoveryRefusalNoMutation(t, f); state.Safety != "blocked_recover_unverified_head" {
			t.Fatalf("moved-gate refusal = %s", state.Safety)
		}
		if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != outside {
			t.Fatalf("refusal moved the gate branch to %s", got)
		}
	})
	t.Run("rebased preserved head", func(t *testing.T) {
		t.Parallel()
		f, _ := newStrandedTestFixFixture(t, strandedTestFixOptions{rebased: true})
		if isAncestor(f.ctx, f.gate, f.submitted, f.preserved) {
			t.Fatal("fixture recorded head still descends from the submitted head")
		}
		if state := assertUnverifiedRecoveryRefusalNoMutation(t, f); state.Safety != "blocked_recover_unverified_head" {
			t.Fatalf("rebased refusal = %s", state.Safety)
		}
	})
	t.Run("preserved head pruned from the gate", func(t *testing.T) {
		t.Parallel()
		f, _ := newStrandedTestFixFixture(t, strandedTestFixOptions{})
		mustRun(t, f.gate, "prune", "--expire=now")
		if objectExists(f.ctx, f.gate, f.preserved) {
			t.Fatal("fixture gate still holds the recorded head after pruning")
		}
		if state := assertUnverifiedRecoveryRefusalNoMutation(t, f); state.Safety != "blocked_recover_unverified_head" {
			t.Fatalf("pruned-head refusal = %s", state.Safety)
		}
	})
	t.Run("conflicting create-only anchor", func(t *testing.T) {
		t.Parallel()
		f, _ := newStrandedTestFixFixture(t, strandedTestFixOptions{})
		mustRun(t, f.gate, "update-ref", f.anchorRef(), f.submitted)
		if state := assertUnverifiedRecoveryRefusalNoMutation(t, f); state.Safety != "blocked_recover_anchor_mismatch" {
			t.Fatalf("anchor-conflict refusal = %s", state.Safety)
		}
	})
	t.Run("racing local branch", func(t *testing.T) {
		t.Parallel()
		f, _ := newStrandedTestFixFixture(t, strandedTestFixOptions{})
		f.service.beforeRecoverTerminalHeadPreserve = func() {
			mustRun(t, f.local, "checkout", "-b", "racing-branch", f.submitted)
		}
		if state := assertUnverifiedRecoveryRefusalNoMutation(t, f); state.Safety != "blocked_recover_assumptions_changed" {
			t.Fatalf("branch-race refusal = %s", state.Safety)
		}
		if got := mustRun(t, f.local, "branch", "--show-current"); got != "racing-branch" {
			t.Fatalf("racing branch was changed to %s", got)
		}
	})
	t.Run("review authority changed under the proof", func(t *testing.T) {
		t.Parallel()
		f, _ := newStrandedTestFixFixture(t, strandedTestFixOptions{})
		outside := commitOutsidePreservedHistory(t, f)
		f.service.beforeRecoverTerminalHeadPreserve = func() {
			if err := f.db.UpdateRunReviewApprovedHeadSHA(f.run.ID, outside); err != nil {
				t.Fatal(err)
			}
		}
		state := f.service.Recover(f.ctx, false)
		if state.Recovered || state.Changed || state.Safety != "blocked_recover_assumptions_changed" {
			t.Fatalf("stale-proof recovery = %#v", state)
		}
		run, err := f.db.GetRun(f.run.ID)
		if err != nil || run.TerminalHeadVerifiedAt != nil || run.CustodyReturnedAt != nil || run.HeadSHA != f.preserved {
			t.Fatalf("stale proof changed the run: %#v, %v", run, err)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
			t.Fatalf("stale proof moved HEAD to %s", got)
		}
	})
}
