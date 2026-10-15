package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// replayFixture is a CI repair fixture whose reviewed branch carries two
// commits, with the local base advanced by one commit the branch never saw,
// so a test can rewrite the branch onto that base the way a rebase repair
// does and observe which way the repair is recorded.
type replayFixture struct {
	*ciRepairFixture
	reviewedHead string
	base         string
}

func newReplayFixture(t *testing.T, carry, revalidate bool) *replayFixture {
	t.Helper()
	f := newCIRepairFixture(t, revalidate, nil)
	f.sctx.Config.CI.CarryReviewOnReplay = carry

	// A second reviewed commit, so the proof has a chain to walk.
	writeFile(t, f.dir, "feature2.txt", "second reviewed change\n")
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "feature part two")
	reviewed := gitCmd(t, f.dir, "rev-parse", "HEAD")
	gitCmd(t, f.dir, "push", "origin", "feature")
	if err := f.sctx.DB.UpdateRunHeadSHA(f.sctx.Run.ID, reviewed); err != nil {
		t.Fatal(err)
	}
	f.sctx.Run.HeadSHA = reviewed
	recordReviewApproval(t, f.sctx, reviewed)
	f.headSHA = reviewed

	// The base moves on, touching nothing the branch touches.
	gitCmd(t, f.dir, "checkout", "main")
	writeFile(t, f.dir, "base.txt", "another PR landed\n")
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "base advances")
	base := gitCmd(t, f.dir, "rev-parse", "HEAD")
	gitCmd(t, f.dir, "checkout", "feature")
	return &replayFixture{ciRepairFixture: f, reviewedHead: reviewed, base: base}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// record runs the CI repair recording exactly as a rebase repair round does:
// with the resolved rebase target bound for the proof.
func (f *replayFixture) record(t *testing.T) ciRepairResult {
	t.Helper()
	repair, err := (&CIStep{replayBase: f.base}).commitRepair(f.sctx, "rebase onto the base", nil)
	if err != nil {
		t.Fatalf("recording the rebase repair failed: %v\nlog:\n%s", err, f.log())
	}
	if !repair.HeadAdvanced || !repair.Revalidate {
		t.Fatalf("repair = %+v, want a held, advanced head", repair)
	}
	return repair
}

func (f *replayFixture) approval(t *testing.T) string {
	t.Helper()
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ReviewApprovedHeadSHA == nil {
		return ""
	}
	return strings.TrimSpace(*run.ReviewApprovedHeadSHA)
}

// The behavioral core of ci.carry_review_on_replay: the same advanced base,
// rewritten in different ways, and the only rewrite that keeps the review
// approval is an unattended replay of the exact reviewed commits.
func TestCIStep_CarryReviewOnReplay_OnlyAPureReplayKeepsTheApproval(t *testing.T) {
	t.Parallel()
	cleanRebase := func(t *testing.T, f *replayFixture) {
		gitCmd(t, f.dir, "rebase", f.base)
	}
	for _, tc := range []struct {
		name              string
		carry, revalidate bool
		gates             []config.Gate
		rewrite           func(t *testing.T, f *replayFixture)
		wantCarried       bool
		wantLog           string
	}{
		{
			name:  "pure_replay_carries_the_approval_and_restarts_at_test",
			carry: true, rewrite: cleanRebase, wantCarried: true,
			wantLog: "rebase repair is a pure replay of reviewed head",
		},
		{
			name:  "flag_off_is_todays_restart_at_review",
			carry: false, rewrite: cleanRebase,
			wantLog: "cannot prove the repaired head continues the reviewed head",
		},
		{
			name:  "revalidate_repairs_wins_over_the_flag",
			carry: true, revalidate: true, rewrite: cleanRebase,
			wantLog: "committed CI repair for revalidation",
		},
		{
			name:  "review_anchored_gate_keeps_the_full_restart",
			carry: true, rewrite: cleanRebase,
			gates:   []config.Gate{{Name: "audit", After: types.StepReview, Command: "true"}},
			wantLog: `gate "audit" runs after Review`,
		},
		{
			name:  "changed_patch_is_not_a_replay",
			carry: true,
			rewrite: func(t *testing.T, f *replayFixture) {
				cleanRebase(t, f)
				// Same commit count and parentage, but the reviewed change
				// itself was edited while it was being replayed.
				writeFile(t, f.dir, "feature2.txt", "second reviewed change, edited\n")
				gitCmd(t, f.dir, "commit", "-a", "--amend", "--no-edit")
			},
			wantLog: "is not the conflict-free replay of reviewed commit",
		},
		{
			name:  "whitespace_only_change_is_not_a_replay",
			carry: true,
			rewrite: func(t *testing.T, f *replayFixture) {
				cleanRebase(t, f)
				writeFile(t, f.dir, "feature2.txt", "second reviewed change \n")
				gitCmd(t, f.dir, "commit", "-a", "--amend", "--no-edit")
			},
			wantLog: "is not the conflict-free replay of reviewed commit",
		},
		{
			name:  "extra_commit_on_top_is_not_a_replay",
			carry: true,
			rewrite: func(t *testing.T, f *replayFixture) {
				cleanRebase(t, f)
				// Left uncommitted: the repair recorder commits it on top.
				writeCIFix(f.dir)
			},
			wantLog: "2 reviewed commits but 3 commits on the repaired head",
		},
		{
			name:  "squashed_commits_are_not_a_replay",
			carry: true,
			rewrite: func(t *testing.T, f *replayFixture) {
				cleanRebase(t, f)
				gitCmd(t, f.dir, "reset", "--soft", "HEAD~2")
				gitCmd(t, f.dir, "commit", "-m", "squashed")
			},
			wantLog: "2 reviewed commits but 1 commits on the repaired head",
		},
		{
			name:  "dropped_commit_is_not_a_replay",
			carry: true,
			rewrite: func(t *testing.T, f *replayFixture) {
				gitCmd(t, f.dir, "rebase", "--onto", f.base, "HEAD~1")
			},
			wantLog: "2 reviewed commits but 1 commits on the repaired head",
		},
		{
			name:  "merge_instead_of_rebase_is_not_a_replay",
			carry: true,
			rewrite: func(t *testing.T, f *replayFixture) {
				// The replayed commits brought in by a merge on top of the
				// base: same content, but not a linear chain on the base.
				gitCmd(t, f.dir, "rebase", f.base)
				replayed := gitCmd(t, f.dir, "rev-parse", "HEAD")
				gitCmd(t, f.dir, "reset", "--hard", f.base)
				gitCmd(t, f.dir, "merge", "--no-ff", "-m", "merge replayed", replayed)
			},
			wantLog: "is not a linear chain",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newReplayFixture(t, tc.carry, tc.revalidate)
			f.sctx.Config.Gates = tc.gates
			tc.rewrite(t, f)
			repaired := f.localHead(t)
			if repaired == f.reviewedHead {
				t.Fatal("the rewrite did not move the reviewed head")
			}

			repair := f.record(t)
			repaired = f.localHead(t)

			if repair.ReviewCarried != tc.wantCarried {
				t.Errorf("ReviewCarried = %v, want %v", repair.ReviewCarried, tc.wantCarried)
			}
			wantRestart := types.StepReview
			wantApproval := ""
			if tc.wantCarried {
				wantRestart = types.StepTest
				wantApproval = repaired
			}
			if got := repair.restartFrom(); got != wantRestart {
				t.Errorf("restart from %s, want %s", got, wantRestart)
			}
			if got := f.approval(t); got != wantApproval {
				t.Errorf("durable review approval = %q, want %q", got, wantApproval)
			}
			if f.sctx.Run.ReviewApprovedHeadSHA == nil && wantApproval != "" ||
				f.sctx.Run.ReviewApprovedHeadSHA != nil && *f.sctx.Run.ReviewApprovedHeadSHA != wantApproval && wantApproval != "" {
				t.Errorf("in-memory approval disagrees with the durable one")
			}
			if f.sctx.Run.HeadSHA != repaired {
				t.Errorf("recorded head = %s, want the repaired head %s", f.sctx.Run.HeadSHA, repaired)
			}
			// Either way nothing is published: Push re-publishes after the
			// restart, so the reviewed head stays on the remote meanwhile.
			if f.remoteHead(t) != f.reviewedHead {
				t.Errorf("remote moved to %s; the repair must stay local until Push", f.remoteHead(t))
			}
			if !strings.Contains(f.log(), tc.wantLog) {
				t.Errorf("log missing %q:\n%s", tc.wantLog, f.log())
			}
		})
	}
}

// A rebase that stopped on a conflict and was resolved is the case #826 is
// about: the resolution is code nobody has reviewed, so the approval never
// carries, even though the resolved head is a linear chain on the base.
func TestCIStep_CarryReviewOnReplay_ResolvedConflictRevalidatesFromReview(t *testing.T) {
	t.Parallel()
	f := newReplayFixture(t, true, false)
	gitCmd(t, f.dir, "checkout", "main")
	writeFile(t, f.dir, "feature.txt", "base rewrote this line\n")
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "base edits the same line")
	f.base = gitCmd(t, f.dir, "rev-parse", "HEAD")
	gitCmd(t, f.dir, "checkout", "feature")
	if _, err := stepGitRun(f.sctx, "rebase", f.base); err == nil {
		t.Fatal("expected the rebase to conflict")
	}
	writeFile(t, f.dir, "feature.txt", "base rewrote this line\nfeature\n")
	gitCmd(t, f.dir, "add", "-A")
	if _, err := stepGitRun(f.sctx, "-c", "core.editor=true", "rebase", "--continue"); err != nil {
		t.Fatalf("resolve the conflict: %v", err)
	}

	repair := f.record(t)

	if repair.ReviewCarried || repair.restartFrom() != types.StepReview {
		t.Fatalf("repair = %+v, want a restart from Review", repair)
	}
	if got := f.approval(t); got != "" {
		t.Fatalf("a resolved conflict kept review approval %s", got)
	}
	if !strings.Contains(f.log(), "does not replay onto") {
		t.Errorf("the log does not say the commit conflicts:\n%s", f.log())
	}
}

// git rebase can complete unaided while the base edited lines adjacent to a
// reviewed hunk; the replayed patch then differs (its context moved), so it is
// not patch-identical and the approval does not carry. This pins the
// documented boundary of the proof: it is deliberately narrower than "git
// rebase did not stop".
func TestCIStep_CarryReviewOnReplay_CleanRebaseWithShiftedContextIsNotPatchIdentical(t *testing.T) {
	t.Parallel()
	f := newReplayFixture(t, true, false)
	// Rebuild the branch around a file both sides edit, a few lines apart.
	gitCmd(t, f.dir, "checkout", "main")
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = "line"
	}
	lines = append(lines, "")
	writeFile(t, f.dir, "shared.txt", strings.Join(lines, "\n"))
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "shared file")
	gitCmd(t, f.dir, "checkout", "feature")
	gitCmd(t, f.dir, "rebase", "main")
	edited := append([]string(nil), lines...)
	edited[10] = "feature edit"
	writeFile(t, f.dir, "shared.txt", strings.Join(edited, "\n"))
	gitCmd(t, f.dir, "commit", "-a", "-m", "feature edits shared")
	reviewed := gitCmd(t, f.dir, "rev-parse", "HEAD")
	if err := f.sctx.DB.UpdateRunHeadSHA(f.sctx.Run.ID, reviewed); err != nil {
		t.Fatal(err)
	}
	f.sctx.Run.HeadSHA = reviewed
	recordReviewApproval(t, f.sctx, reviewed)
	f.reviewedHead = reviewed
	gitCmd(t, f.dir, "push", "-f", "origin", "feature")

	gitCmd(t, f.dir, "checkout", "main")
	baseEdit := append([]string(nil), lines...)
	baseEdit[8] = "base edit"
	writeFile(t, f.dir, "shared.txt", strings.Join(baseEdit, "\n"))
	gitCmd(t, f.dir, "commit", "-a", "-m", "base edits nearby")
	f.base = gitCmd(t, f.dir, "rev-parse", "HEAD")
	gitCmd(t, f.dir, "checkout", "feature")
	gitCmd(t, f.dir, "rebase", f.base)

	repair := f.record(t)

	if repair.ReviewCarried {
		t.Fatalf("a replay whose patch changed carried the approval: %+v", repair)
	}
	if !strings.Contains(f.log(), "is not patch-identical to reviewed commit") {
		t.Errorf("the log does not name the patch mismatch:\n%s", f.log())
	}
}

// The approval is carried only from the approval the proof was made against:
// one revoked meanwhile is never resurrected onto the replayed head.
func TestCIStep_CarryReviewOnReplay_NeverResurrectsARevokedApproval(t *testing.T) {
	t.Parallel()
	f := newReplayFixture(t, true, false)
	gitCmd(t, f.dir, "rebase", f.base)
	replayed := f.localHead(t)
	if err := f.sctx.DB.UpdateRunReviewApprovedHeadSHA(f.sctx.Run.ID, f.base); err != nil {
		t.Fatal(err)
	}
	carried, err := f.sctx.DB.UpdateRunHeadSHAWithCarriedReview(f.sctx.Run.ID, f.reviewedHead, replayed)
	if err != nil {
		t.Fatal(err)
	}
	if carried {
		t.Fatal("carried an approval the run no longer holds")
	}
	if got := f.approval(t); got != f.base {
		t.Fatalf("approval = %s, want the unrelated approval left alone", got)
	}
}

// End to end through the CI monitor: a green PR GitHub reports BEHIND takes
// the rebase repair, and with the flag on a clean replay restarts at Test with
// the approval on the rebased head instead of restarting at Review.
func TestCIStep_CarryReviewOnReplay_BehindPRRestartsAtTest(t *testing.T) {
	t.Parallel()
	var prompts []string
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompts = append(prompts, opts.Prompt)
		gitCmd(t, opts.CWD, "fetch", "origin", "main")
		gitCmd(t, opts.CWD, "rebase", "origin/main")
		return &agent.Result{}, nil
	}}
	sctx, logs := behindBaseContext(t, ag, "MERGEABLE BEHIND")
	sctx.Config.CI.CarryReviewOnReplay = true
	originalHead := sctx.Run.HeadSHA
	other := t.TempDir()
	gitCmd(t, other, "clone", "-b", "main", sctx.Repo.UpstreamURL, ".")
	gitCmd(t, other, "config", "user.name", "test")
	gitCmd(t, other, "config", "user.email", "test@test.com")
	writeFile(t, other, "base-advance.txt", "x")
	gitCmd(t, other, "add", "-A")
	gitCmd(t, other, "commit", "-m", "base advances")
	gitCmd(t, other, "push", "origin", "main")
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
		t.Fatalf("driveCI: %v; logs: %v", err, *logs)
	}
	if len(prompts) != 1 {
		t.Fatalf("rebase repair ran %d times, want once; logs: %v", len(prompts), *logs)
	}
	if outcome == nil || outcome.RestartFrom != types.StepTest {
		t.Fatalf("outcome = %+v, want a restart from Test; logs: %v", outcome, *logs)
	}
	newHead := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
	if newHead == originalHead || sctx.Run.HeadSHA != newHead {
		t.Fatalf("run head = %s, local = %s, want the rebased head", sctx.Run.HeadSHA, newHead)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != newHead {
		t.Fatalf("review approval = %v, want it carried to %s", run.ReviewApprovedHeadSHA, newHead)
	}
	if remote := gitCmd(t, sctx.WorkDir, "ls-remote", "origin", "refs/heads/feature"); !strings.HasPrefix(remote, originalHead) {
		t.Fatalf("rebased head was pushed before Test ran; remote = %q", remote)
	}
}

// carry records a pure replay that carried the approval, as a rebase repair
// round does, and returns the replayed head.
func (f *replayFixture) carry(t *testing.T) string {
	t.Helper()
	gitCmd(t, f.dir, "rebase", f.base)
	if repair := f.record(t); !repair.ReviewCarried {
		t.Fatalf("setup: the pure replay did not carry; log:\n%s", f.log())
	}
	return f.localHead(t)
}

func (f *replayFixture) testRestartPending(t *testing.T) bool {
	t.Helper()
	pending, err := f.sctx.DB.RunTestRestartPending(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return pending
}

// The restart at Test a carry asks for must outlive the outcome that asked
// for it. CI parked on ask-user findings and answered with a fix whose fixer
// changed nothing reaches the repair recorder with the repaired head equal to
// the (carried) approved head: that must restart at Test again, never publish
// the carried head untested. Only a publication - the Push step's, once Test
// onward ran - settles it.
func TestCIStep_CarryReviewOnReplay_ALaterNoChangeFixStillRestartsAtTest(t *testing.T) {
	t.Parallel()
	f := newReplayFixture(t, true, false)
	replayed := f.carry(t)
	if !f.testRestartPending(t) {
		t.Fatal("the carry did not record its pending Test restart")
	}

	repair, err := (&CIStep{}).commitRepair(f.sctx, "", nil)
	if err != nil {
		t.Fatalf("no-change fix round: %v\nlog:\n%s", err, f.log())
	}

	if !repair.Revalidate || repair.restartFrom() != types.StepTest {
		t.Fatalf("repair = %+v, want a restart from Test", repair)
	}
	if f.remoteHead(t) != f.reviewedHead {
		t.Fatalf("remote moved to %s; the carried head was published before Test ran", f.remoteHead(t))
	}
	if got := f.approval(t); got != replayed {
		t.Fatalf("approval = %q, want it still on the carried head %s", got, replayed)
	}
	if !f.testRestartPending(t) {
		t.Fatal("the pending Test restart was dropped before Test ran")
	}

	if err := f.sctx.DB.UpdateRunPublication(f.sctx.Run.ID, db.PushBinding{HeadSHA: replayed, TargetKind: "upstream", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	if f.testRestartPending(t) {
		t.Fatal("publishing the carried head did not settle its pending Test restart")
	}
}

// A fix round that commits on top of a carried head that never went through
// Test is not published either; it revalidates from Review, as the rebase
// would have with the carry off.
func TestCIStep_CarryReviewOnReplay_ACommitOnTopOfAPendingCarryRevalidatesFromReview(t *testing.T) {
	t.Parallel()
	f := newReplayFixture(t, true, false)
	f.carry(t)
	writeCIFix(f.dir)

	repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check", nil)
	if err != nil {
		t.Fatalf("fix round: %v\nlog:\n%s", err, f.log())
	}

	if !repair.Revalidate || repair.restartFrom() != types.StepReview {
		t.Fatalf("repair = %+v, want a restart from Review", repair)
	}
	if f.remoteHead(t) != f.reviewedHead {
		t.Fatalf("remote moved to %s; a repair on an untested carried head was published", f.remoteHead(t))
	}
	if got := f.approval(t); got != "" {
		t.Fatalf("approval = %q, want it revoked", got)
	}
}

// Stale checks from the old published head cannot authorize a fixer while a
// carried head is pending; the step waits and restarts Test instead.
func TestCIStep_CarryReviewOnReplay_StaleChecksOnPendingCarryRestartTest(t *testing.T) {
	t.Parallel()
	f := newReplayFixture(t, true, false)
	f.carry(t)
	f.sctx.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Error("fixer called using stale checks from the old published head")
		return nil, errors.New("unexpected fixer call")
	}}
	f.sctx.Fixing = true
	f.sctx.PreviousFindings = ciGateFindingsJSON("test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.sctx.Ctx = ctx

	outcome, err := (&CIStep{observedCompletedAt: map[string]checkFreshness{}, waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}}).Execute(f.sctx)
	if err != nil {
		t.Fatalf("Execute: %v\nlog:\n%s", err, f.log())
	}
	if outcome == nil || outcome.RestartFrom != types.StepTest {
		t.Fatalf("outcome = %+v, want a restart from Test\nlog:\n%s", outcome, f.log())
	}
	if f.remoteHead(t) != f.reviewedHead {
		t.Fatalf("remote moved to %s; the carried head was published before Test ran", f.remoteHead(t))
	}
}

// An independent merge-conflict finding legitimately invokes the fixer while
// a carry is pending. A fixer that commits on top of that head and then fails
// must not have that commit restart at Test under the carried approval: Test
// and Push both accept a descendant, so the unreviewed commit would publish.
// It is recorded like any other repair on a pending carry: approval revoked,
// revalidation from Review.
func TestCIStep_CarryReviewOnReplay_AFailedFixerCommitOnTopRevalidatesFromReview(t *testing.T) {
	t.Parallel()
	f := newReplayFixture(t, true, false)
	f.carry(t)
	f.sctx.Agent = &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		writeCIFix(opts.CWD)
		gitCmd(t, opts.CWD, "add", "-A")
		gitCmd(t, opts.CWD, "commit", "-m", "partial fix")
		return nil, errors.New("fixer crashed after committing")
	}}
	f.sctx.Fixing = true
	findings := Findings{Summary: "CI issues", Items: []Finding{{
		ID: "ci-conflict", Severity: types.FindingSeverityError,
		Action: types.ActionAutoFix, Category: types.FindingCategoryCIMergeConflict,
		Description: "PR has merge conflicts with the base branch",
	}}}
	var err error
	f.sctx.PreviousFindings, err = types.MarshalFindingsJSON(findings)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.sctx.Ctx = ctx

	outcome, err := (&CIStep{observedCompletedAt: map[string]checkFreshness{}, waitForNextPoll: func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}}).Execute(f.sctx)
	if err != nil {
		t.Fatalf("Execute: %v\nlog:\n%s", err, f.log())
	}
	if outcome == nil || outcome.RestartFrom != types.StepReview {
		t.Fatalf("outcome = %+v, want a restart from Review\nlog:\n%s", outcome, f.log())
	}
	committed := f.localHead(t)
	if f.sctx.Run.HeadSHA != committed {
		t.Fatalf("run head = %s, want the fixer's commit %s recorded", f.sctx.Run.HeadSHA, committed)
	}
	if got := f.approval(t); got != "" {
		t.Fatalf("approval = %q, want it revoked", got)
	}
	if f.remoteHead(t) != f.reviewedHead {
		t.Fatalf("remote moved to %s; the failed fixer's commit was published", f.remoteHead(t))
	}
}
