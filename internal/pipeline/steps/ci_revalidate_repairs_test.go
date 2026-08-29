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
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ciRepairFixture is one CI monitor run wired to a real git worktree, a real
// bare upstream, and a fake gh reporting one failing check, so a test can
// observe what a repair does to the local head, the remote, and the run's
// review authority. Tests using this process-heavy fixture intentionally run
// serially: running all of their git and fake-gh subprocesses in parallel can
// exhaust macOS CI process capacity and stall a child indefinitely.
type ciRepairFixture struct {
	sctx     *pipeline.StepContext
	dir      string
	upstream string
	headSHA  string
	gateDir  string
	logs     *[]string
}

func newCIRepairFixture(t *testing.T, revalidate bool, agentAction func(workDir string)) *ciRepairFixture {
	t.Helper()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if agentAction != nil {
			agentAction(opts.CWD)
		}
		return &agent.Result{Output: []byte(`{"summary":"repair the failing check"}`)}, nil
	}}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"test","state":"FAILURE","bucket":"fail"}]`)
	sctx.Run.PRURL = &prURL
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 1}
	sctx.Config.CI.RevalidateRepairs = revalidate

	// The CI step only ever runs after Push succeeded, so a run always reaches
	// it with a durable review approval and a recorded push binding.
	if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, headSHA); err != nil {
		t.Fatal(err)
	}
	sctx.Run.ReviewApprovedHeadSHA = &headSHA
	if err := sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{
		HeadSHA: headSHA, TargetKind: "upstream",
		TargetFingerprint: branchsync.TargetFingerprint(upstream), Ref: "refs/heads/feature",
	}); err != nil {
		t.Fatal(err)
	}

	logs := &[]string{}
	sctx.Log = func(s string) { *logs = append(*logs, s) }
	return &ciRepairFixture{sctx: sctx, dir: dir, upstream: upstream, headSHA: headSHA, gateDir: sctx.GateDir, logs: logs}
}

// run drives the monitor until it returns or the poll budget is spent.
func (f *ciRepairFixture) run(t *testing.T) (*pipeline.StepOutcome, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.sctx.Ctx = ctx
	polls := 0
	step := &CIStep{waitForNextPoll: func(ctx context.Context, d time.Duration) error {
		polls++
		if polls >= 2 {
			cancel()
		}
		return ctx.Err()
	}}
	return step.Execute(f.sctx)
}

func (f *ciRepairFixture) localHead(t *testing.T) string {
	return gitCmd(t, f.dir, "rev-parse", "HEAD")
}
func (f *ciRepairFixture) remoteHead(t *testing.T) string {
	return gitCmd(t, f.upstream, "rev-parse", "refs/heads/feature")
}
func (f *ciRepairFixture) log() string { return strings.Join(*f.logs, "\n") }

func writeCIFix(workDir string) {
	os.WriteFile(filepath.Join(workDir, "ci-fix.txt"), []byte("fixed"), 0o644)
}

// TestCIStep_RevalidateRepairsPolicySelectsRepairDelivery is the behavioral
// core of ci.revalidate_repairs: the same failing check, the same repair, and
// two entirely different deliveries.
func TestCIStep_RevalidateRepairsPolicySelectsRepairDelivery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		revalidate       bool
		wantRestart      bool
		wantRemoteMoved  bool
		wantApprovalKept bool
		wantLog          string
	}{
		{
			name:       "default_publishes_the_repair_and_keeps_monitoring",
			revalidate: false, wantRestart: false, wantRemoteMoved: true, wantApprovalKept: true,
			wantLog: "committed and pushed CI repair",
		},
		{
			name:       "opt_in_holds_the_repair_and_restarts_at_review",
			revalidate: true, wantRestart: true, wantRemoteMoved: false, wantApprovalKept: false,
			wantLog: "committed CI repair for revalidation",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCIRepairFixture(t, tc.revalidate, nil)
			writeCIFix(f.dir)
			// commitRepair, not the whole monitor loop: the delivery decision
			// is what this table is about, and driving Execute here spends a
			// provider poll and several subprocesses per case for nothing.
			// TestCIStep_MonitorRestartsAtReviewForAHeldRepair covers the
			// monitor turning Revalidate into RestartFrom.
			repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check")
			if err != nil {
				t.Fatalf("CI repair returned error: %v\nlog:\n%s", err, f.log())
			}
			if !repair.HeadAdvanced {
				t.Fatal("the repair was not recorded as a real change")
			}
			if repair.Revalidate != tc.wantRestart {
				t.Errorf("Revalidate = %v, want %v", repair.Revalidate, tc.wantRestart)
			}

			localHead := f.localHead(t)
			if localHead == f.headSHA {
				t.Fatal("the repair commit was never created")
			}

			remoteMoved := f.remoteHead(t) != f.headSHA
			if remoteMoved != tc.wantRemoteMoved {
				t.Errorf("remote advanced = %v, want %v", remoteMoved, tc.wantRemoteMoved)
			}
			if tc.wantRemoteMoved && f.remoteHead(t) != localHead {
				t.Errorf("remote head = %s, want the repair commit %s", f.remoteHead(t), localHead)
			}

			run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			approvalKept := run.ReviewApprovedHeadSHA != nil && strings.TrimSpace(*run.ReviewApprovedHeadSHA) != ""
			if approvalKept != tc.wantApprovalKept {
				t.Errorf("review approval retained = %v, want %v", approvalKept, tc.wantApprovalKept)
			}
			if run.HeadSHA != localHead {
				t.Errorf("recorded head = %s, want the repair commit %s", run.HeadSHA, localHead)
			}

			// A published repair must record the delivery; a held one must not
			// claim one.
			publishedSHA := ""
			if run.LastPushedSHA != nil {
				publishedSHA = *run.LastPushedSHA
			}
			if tc.wantRemoteMoved && publishedSHA != localHead {
				t.Errorf("push binding = %s, want the published repair %s", publishedSHA, localHead)
			}
			if !tc.wantRemoteMoved && publishedSHA == localHead {
				t.Error("a repair held for revalidation was recorded as published")
			}

			if !strings.Contains(f.log(), tc.wantLog) {
				t.Errorf("log missing %q; got:\n%s", tc.wantLog, f.log())
			}
			t.Logf("observable delivery: revalidate=%t prior_head=%s local_head=%s remote_head=%s approval_retained=%t published_head=%s\nCI log:\n%s",
				repair.Revalidate, f.headSHA, localHead, f.remoteHead(t), approvalKept, publishedSHA, f.log())
		})
	}
}

// A repair the agent declined to make is not a repair under either policy: no
// commit, no publication, no restart, and the attempt budget still decides
// when to stop.
func TestCIStep_NoChangeRepairNeitherPublishesNorRestarts(t *testing.T) {
	t.Parallel()
	for _, revalidate := range []bool{false, true} {
		revalidate := revalidate
		name := "publish_policy"
		if revalidate {
			name = "revalidate_policy"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newCIRepairFixture(t, revalidate, nil)
			repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check")
			if err != nil {
				t.Fatalf("CI repair returned error: %v", err)
			}
			if repair.HeadAdvanced || repair.Revalidate {
				t.Errorf("a no-change repair was reported as a delivery: %#v", repair)
			}
			if f.localHead(t) != f.headSHA {
				t.Error("a no-change repair created a commit")
			}
			if f.remoteHead(t) != f.headSHA {
				t.Error("a no-change repair published something")
			}
			if !strings.Contains(f.log(), "no changes to commit") {
				t.Errorf("log missing the no-change outcome; got:\n%s", f.log())
			}
		})
	}
}

// The agent may commit the repair itself - the merge-conflict and
// `git rebase --continue` shape leaves a clean worktree with an advanced HEAD.
// Both policies must recognize that as a real repair and deliver it their own
// way, rather than reading the clean tree as "nothing happened".
func TestCIStep_AgentCommittedRepairFollowsThePolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		revalidate      bool
		wantRestart     bool
		wantRemoteMoved bool
	}{
		{name: "publish_policy", revalidate: false, wantRestart: false, wantRemoteMoved: true},
		{name: "revalidate_policy", revalidate: true, wantRestart: true, wantRemoteMoved: false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newCIRepairFixture(t, tc.revalidate, nil)
			// The agent commits the repair itself and leaves a clean tree.
			os.WriteFile(filepath.Join(f.dir, "resolved.txt"), []byte("resolved"), 0o644)
			gitCmd(t, f.dir, "add", "-A")
			gitCmd(t, f.dir, "commit", "-m", "agent resolved the failure")
			repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check")
			if err != nil {
				t.Fatalf("CI repair returned error: %v\nlog:\n%s", err, f.log())
			}
			if f.localHead(t) == f.headSHA {
				t.Fatal("the agent's own commit was not detected")
			}
			if repair.Revalidate != tc.wantRestart {
				t.Errorf("Revalidate = %v, want %v", repair.Revalidate, tc.wantRestart)
			}
			if moved := f.remoteHead(t) != f.headSHA; moved != tc.wantRemoteMoved {
				t.Errorf("remote advanced = %v, want %v", moved, tc.wantRemoteMoved)
			}
		})
	}
}

// Publication is all-or-nothing: the remote push, the gate mirror, the push
// binding, and the recorded head either all land or none of them are recorded.
// A gate-mirror failure happens after the remote already carries the head, so
// the tempting shortcut is to record the publication anyway. Recording it would
// leave the gate behind the remote, where `no-mistakes rerun` resolves the
// stale gate head and silently omits the repair.
//
// Nothing is recorded until every part succeeds, so the failure is simply
// something the next fix attempt re-enters and completes.
func TestCIStep_PartialPublicationRecordsNothing(t *testing.T) {
	t.Parallel()
	f := newCIRepairFixture(t, false, nil)
	writeCIFix(f.dir)
	brokenGate := filepath.Join(t.TempDir(), "invalid-gate")
	if err := os.MkdirAll(brokenGate, 0o755); err != nil {
		t.Fatal(err)
	}
	f.sctx.GateDir = brokenGate

	repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check")
	if err == nil {
		t.Fatal("a publication that could not settle the gate mirror was reported as complete")
	}
	if repair.HeadAdvanced {
		t.Fatal("an unsettled publication was reported as a delivered repair")
	}

	repairCommit := f.localHead(t)
	if repairCommit == f.headSHA {
		t.Fatal("the repair commit was never created")
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != f.headSHA {
		t.Errorf("recorded head = %s, want the pre-repair head %s until publication settles", run.HeadSHA, f.headSHA)
	}
	if run.LastPushedSHA != nil && *run.LastPushedSHA == repairCommit {
		t.Error("an unsettled publication was recorded in the push binding")
	}

	// With a working gate the same path completes, and the no-op push over the
	// already-pushed head is not an obstacle.
	f.sctx.GateDir = f.gateDir
	repair, err = (&CIStep{}).commitRepair(f.sctx, "repair the failing check")
	if err != nil {
		t.Fatalf("the next attempt did not complete the publication: %v\nlog:\n%s", err, f.log())
	}
	if !repair.HeadAdvanced || repair.Revalidate {
		t.Fatalf("result = %#v, want a published repair", repair)
	}
	if f.remoteHead(t) != repairCommit {
		t.Errorf("remote head = %s, want the repair %s", f.remoteHead(t), repairCommit)
	}
	run, err = f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != repairCommit || run.LastPushedSHA == nil || *run.LastPushedSHA != repairCommit {
		t.Errorf("run did not record the settled publication: head=%s pushed=%v", run.HeadSHA, run.LastPushedSHA)
	}
}

// A merge-conflict repair rewrites history, so its head is never a descendant
// of the reviewed head and its continuity can never be proven. The uniform rule
// therefore sends every conflict repair down the revalidating path - it is not
// carved out, it just always lands in the cannot-be-proven half.
//
// Both directions matter, and both are load bearing:
//   - a genuine conflict rebase must still SUCCEED, revalidating rather than
//     being refused, so conflict repair keeps working;
//   - a repair that reset to the base instead of replaying the branch must not
//     reach the remote, so the reviewed commits survive.
//
// The second case is the reason this rule exists. Reproduced against the
// earlier design, that repair force-pushed the reviewed commits away while
// reporting success - and the actor was the CI repair agent itself, which is
// why provenance cannot substitute for proof.
func TestCIStep_ConflictRepairAlwaysRevalidates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// rewrite leaves the worktree on the repaired head and returns it.
		rewrite func(t *testing.T, f *ciRepairFixture, advancedBase string) string
		// keepsReviewedWork is whether the rewrite actually replayed the
		// reviewed commit onto the new base.
		keepsReviewedWork bool
	}{
		{
			name: "genuine_rebase_replaying_the_reviewed_commit",
			rewrite: func(t *testing.T, f *ciRepairFixture, advancedBase string) string {
				// Resolve the conflict the way a repair agent would: keep the
				// feature's intent on top of the base's rewrite. That changes
				// the commit's patch-id, which is exactly why continuity
				// cannot be proven for a conflict repair.
				if err := os.WriteFile(filepath.Join(f.dir, "feature.txt"), []byte("base rewrote this line\nthe user's feature, resolved\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, f.dir, "add", "-A")
				if _, err := stepGitRun(f.sctx, "-c", "core.editor=true", "rebase", "--continue"); err != nil {
					t.Fatalf("resolve the conflict: %v", err)
				}
				return gitCmd(t, f.dir, "rev-parse", "HEAD")
			},
			keepsReviewedWork: true,
		},
		{
			name: "reset_to_base_dropping_the_reviewed_commit",
			rewrite: func(t *testing.T, f *ciRepairFixture, advancedBase string) string {
				// The repair agent gives up on the conflict and resets to the
				// base, silently discarding the reviewed commit.
				gitCmd(t, f.dir, "rebase", "--abort")
				gitCmd(t, f.dir, "reset", "--hard", advancedBase)
				return advancedBase
			},
			keepsReviewedWork: false,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Publish policy: this is the path that could publish without review.
			// The base and the feature edit the SAME line of the same file, so
			// a rebase genuinely conflicts and the repair really is conflict
			// resolution rather than a clean replay.
			f := newCIRepairFixture(t, false, nil)
			gitCmd(t, f.dir, "checkout", "main")
			if err := os.WriteFile(filepath.Join(f.dir, "feature.txt"), []byte("base rewrote this line\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, f.dir, "add", "-A")
			gitCmd(t, f.dir, "commit", "-m", "advance base over the same line")
			advancedBase := gitCmd(t, f.dir, "rev-parse", "HEAD")
			gitCmd(t, f.dir, "checkout", "feature")
			if _, err := stepGitRun(f.sctx, "rebase", "main"); err == nil {
				t.Fatal("expected the rebase to conflict; the fixture no longer models a conflict repair")
			}

			repairedHead := tc.rewrite(t, f, advancedBase)
			if repairedHead == f.headSHA {
				t.Fatal("the rewrite did not move the reviewed head")
			}

			repair, err := (&CIStep{}).commitRepair(f.sctx, "resolve merge conflict")
			if err != nil {
				t.Fatalf("a conflict repair must revalidate, not fail: %v\nlog:\n%s", err, f.log())
			}
			if !repair.HeadAdvanced {
				t.Fatal("the conflict repair was not recorded as a real change")
			}
			if !repair.Revalidate {
				t.Fatal("a conflict repair was published without revalidating")
			}

			// Nothing rewritten reaches the remote. In the reset case this is
			// exactly what keeps the reviewed commit alive.
			if f.remoteHead(t) != f.headSHA {
				t.Fatalf("remote moved to %s; the reviewed head %s must still be published", f.remoteHead(t), f.headSHA)
			}
			reviewedContent := gitCmd(t, f.dir, "show", f.headSHA+":feature.txt")
			published := gitCmd(t, f.upstream, "show", "refs/heads/feature:feature.txt")
			if published != reviewedContent {
				t.Fatalf("DATA LOSS: published feature.txt = %q, want the reviewed content %q", published, reviewedContent)
			}

			// Review authority is revoked so Push cannot publish the rewritten
			// head until Review approves it again.
			run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.ReviewApprovedHeadSHA != nil && strings.TrimSpace(*run.ReviewApprovedHeadSHA) != "" {
				t.Error("review approval survived a rewritten repair")
			}
			if run.HeadSHA != repairedHead {
				t.Errorf("recorded head = %s, want the repaired head %s", run.HeadSHA, repairedHead)
			}
			if !strings.Contains(f.log(), "cannot prove the repaired head continues the reviewed head") {
				t.Errorf("the log does not say why the repair revalidated:\n%s", f.log())
			}
			t.Logf("observable conflict delivery: reviewed_head=%s repaired_head=%s remote_head=%s reviewed_work_retained=%t restart_from=review approval_revoked=true\nCI log:\n%s",
				f.headSHA, repairedHead, f.remoteHead(t), tc.keepsReviewedWork, f.log())
		})
	}
}

// A manual repair - the one a person authorized by answering the CI gate with
// a fix - takes exactly the same delivery decision as an automatic one. The
// policy is about the cost of revalidating a repair, not about who asked for
// it.
func TestCIStep_ManualRepairFollowsTheSamePolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		revalidate      bool
		wantRestart     bool
		wantRemoteMoved bool
	}{
		{name: "publish_policy", revalidate: false, wantRestart: false, wantRemoteMoved: true},
		{name: "revalidate_policy", revalidate: true, wantRestart: true, wantRemoteMoved: false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newCIRepairFixture(t, tc.revalidate, writeCIFix)
			// Automatic auto-fix off; the user answered the gate with "fix".
			f.sctx.Config.AutoFix = config.AutoFix{CI: 0}
			f.sctx.Fixing = true

			outcome, err := f.run(t)
			// Under the publish policy the monitor deliberately does NOT
			// return after a repair, so it is still polling when the test's
			// poll budget cancels it. That cancellation is the observable
			// "kept monitoring", and it is the point of this case.
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("CI step returned error: %v\nlog:\n%s", err, f.log())
			}
			if tc.wantRestart && err != nil {
				t.Fatalf("the revalidation policy must leave the monitor cleanly, got: %v", err)
			}
			if !tc.wantRestart && !errors.Is(err, context.Canceled) {
				t.Fatalf("the publish policy must keep monitoring after a repair, got outcome %#v err %v", outcome, err)
			}
			if !strings.Contains(f.log(), "manual fix requested") {
				t.Fatalf("expected the manual repair path; log:\n%s", f.log())
			}
			if f.localHead(t) == f.headSHA {
				t.Fatal("the manual repair commit was never created")
			}
			gotRestart := outcome != nil && outcome.RestartFrom == types.StepReview
			if gotRestart != tc.wantRestart {
				t.Errorf("RestartFrom review = %v, want %v (outcome %#v)", gotRestart, tc.wantRestart, outcome)
			}
			if moved := f.remoteHead(t) != f.headSHA; moved != tc.wantRemoteMoved {
				t.Errorf("remote advanced = %v, want %v", moved, tc.wantRemoteMoved)
			}
		})
	}
}

// Continuity is proven against the run's durable review authority, so a run
// that has none cannot prove anything: the repair revalidates rather than
// publishing. Fail closed is the whole point - a missing approval is not a
// reason to skip the check, it is a reason the check cannot pass.
func TestCIStep_RepairWithoutReviewAuthorityRevalidatesRatherThanPublishing(t *testing.T) {
	t.Parallel()
	f := newCIRepairFixture(t, false, nil)
	writeCIFix(f.dir)
	if err := f.sctx.DB.UpdateRunReviewApprovedHeadSHA(f.sctx.Run.ID, ""); err != nil {
		t.Fatal(err)
	}
	f.sctx.Run.ReviewApprovedHeadSHA = nil

	repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check")
	if err != nil {
		t.Fatalf("CI repair returned error: %v", err)
	}
	if !repair.Revalidate {
		t.Fatalf("repair = %#v, want it held for revalidation", repair)
	}
	if f.remoteHead(t) != f.headSHA {
		t.Fatal("a repair was published without a recorded review-approved head")
	}
	if !strings.Contains(f.log(), "run has no durably recorded review-approved head") {
		t.Errorf("the log does not name the missing review authority; log:\n%s", f.log())
	}
}

// Durable state is written before the live head advances, so a failed write
// cannot leave the monitor watching a head the run record does not know about
// while its stale review approval still stands.
func TestCIStep_FailedRevalidationWriteDoesNotAdvanceTheLiveHead(t *testing.T) {
	f := newCIRepairFixture(t, true, nil)
	writeCIFix(f.dir)
	priorHead := f.sctx.Run.HeadSHA
	priorApproval := f.sctx.Run.ReviewApprovedHeadSHA

	// Close the database so the durable revalidation write fails.
	if err := f.sctx.DB.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check"); err == nil {
		t.Fatal("a failed durable write was reported as a recorded repair")
	}
	if f.sctx.Run.HeadSHA != priorHead {
		t.Errorf("live head advanced to %s despite the failed write; want %s", f.sctx.Run.HeadSHA, priorHead)
	}
	if f.sctx.Run.ReviewApprovedHeadSHA != priorApproval {
		t.Error("review approval was revoked in memory despite the failed write")
	}
}

// The delivery decision itself is covered by
// TestCIStep_RevalidateRepairsPolicySelectsRepairDelivery without paying for a
// monitor loop. This one test pays for it once, to pin the remaining wiring:
// the monitor turns a held repair into a restart at Review, and states the
// policy in force before it does anything.
func TestCIStep_MonitorRestartsAtReviewForAHeldRepair(t *testing.T) {
	t.Parallel()
	f := newCIRepairFixture(t, true, writeCIFix)
	outcome, err := f.run(t)
	if err != nil {
		t.Fatalf("CI step returned error: %v\nlog:\n%s", err, f.log())
	}
	if outcome == nil || outcome.RestartFrom != types.StepReview {
		t.Fatalf("outcome = %#v, want a restart from Review", outcome)
	}
	if !strings.Contains(f.log(), "CI repair policy:") {
		t.Errorf("CI step did not report its repair policy; log:\n%s", f.log())
	}
}
