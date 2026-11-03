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
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin"
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
	sctx.Env = append(fakeCIGH(t, "OPEN", `[{"name":"test","state":"FAILURE","bucket":"fail"}]`),
		// The fake resolves the PR's head from the run worktree at read time,
		// the way the provider reports the branch's live head: before the repair
		// it is the run head, after the repair it is the pushed repair head.
		"FAKE_CLI_PR_HEAD_SHA=deadbeef",
		"FAKE_CLI_HEAD_FROM_WORKTREE=1",
		// attestHeadBeforePush discovers the PR via FindPR before every publish
		// (Push and a CI repair alike), so the fixture's fake gh must be able to
		// resolve the same PR the fixture's own persisted PRURL names.
		`FAKE_CLI_PR_LIST_JSON=[{"number":42,"url":"https://github.com/test/repo/pull/42","baseRefName":"main"}]`,
	)
	sctx.Run.PRURL = &prURL
	sctx.Run.Branch = "refs/heads/feature"
	// resolveUpstreamURL prefers the worktree's real "origin" remote (set to
	// the local bare upstream above) for the actual git push, so this
	// GitHub-shaped value only drives provider/host/repo-slug resolution -
	// it must match FAKE_CLI_PR_LIST_JSON above for FindPR's own repo-slug
	// cross-check to accept the discovered PR.
	sctx.Repo.UpstreamURL = "https://github.com/test/repo"
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
	return driveCI(t, step, f.sctx)
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
			repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check", nil)
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
			repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check", nil)
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
			repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check", nil)
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

	repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check", nil)
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
	repair, err = (&CIStep{}).commitRepair(f.sctx, "repair the failing check", nil)
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

			repair, err := (&CIStep{}).commitRepair(f.sctx, "resolve merge conflict", nil)
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
			// Automatic auto-fix off; the user answered the gate with "fix",
			// selecting the failing check's finding.
			f.sctx.Config.AutoFix = config.AutoFix{CI: 0}
			f.sctx.Fixing = true
			f.sctx.PreviousFindings = ciGateFindingsJSON("test")

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
			if !strings.Contains(f.log(), "repairing: test") {
				t.Fatalf("expected the selected finding to be repaired; log:\n%s", f.log())
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

	repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check", nil)
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
	t.Parallel()
	f := newCIRepairFixture(t, true, nil)
	writeCIFix(f.dir)
	priorHead := f.sctx.Run.HeadSHA
	priorApproval := f.sctx.Run.ReviewApprovedHeadSHA

	// Close the database so the durable revalidation write fails.
	if err := f.sctx.DB.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check", nil); err == nil {
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

// The pull request's live base is only an input to a restart at Review. A
// repair that publishes and keeps monitoring never needs it, so a forge that
// cannot report it must not cost the run its repair; a repair that restarts
// still fails closed on the same read.
func TestCIStep_UnreadableLiveBaseOnlyStopsTheRestartingRepair(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		revalidate bool
	}{
		{name: "published_repair_survives_it", revalidate: false},
		{name: "restarting_repair_fails_closed", revalidate: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCIRepairFixture(t, tc.revalidate, writeCIFix)
			f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_PR_BASE=!")

			outcome, err := f.run(t)
			if tc.revalidate {
				if err == nil || !strings.Contains(err.Error(), "read the pull request's live base branch") {
					t.Fatalf("outcome = %#v, err = %v, want the restart to fail closed\nlog:\n%s", outcome, err, f.log())
				}
				return
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("CI step returned error: %v\nlog:\n%s", err, f.log())
			}
			if f.remoteHead(t) == f.headSHA {
				t.Fatalf("the repair was never published; log:\n%s", f.log())
			}
			if !strings.Contains(f.log(), "committed and pushed CI repair") {
				t.Errorf("log missing the published repair:\n%s", f.log())
			}
		})
	}
}

// A base read that fails BEFORE the repair says nothing about the repair, and
// by the time the restart needs the base the repair has already committed and
// cleared the review approval. Failing the run there left a recorded head no
// Review had validated with no revalidation scheduled, so the read is re-tried
// at the restart instead and the run restarts at Review with the live base.
func TestCIStep_TransientBaseReadStillRestartsTheRepairAtReview(t *testing.T) {
	t.Parallel()

	// The outage covers every base read up to and including the pre-repair
	// one; the fix agent ends it, so only the read at the restart succeeds.
	forgeBack := filepath.Join(t.TempDir(), "forge-back")
	f := newCIRepairFixture(t, true, func(workDir string) {
		writeCIFix(workDir)
		if err := os.WriteFile(forgeBack, []byte("reachable\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_PR_BASE=main", "FAKE_CLI_PR_BASE_OK_AFTER="+forgeBack)

	outcome, err := f.run(t)
	if err != nil {
		t.Fatalf("CI step returned error: %v\nlog:\n%s", err, f.log())
	}
	if outcome == nil || outcome.RestartFrom != types.StepReview {
		t.Fatalf("outcome = %#v, want a restart from Review despite the first base read failing\nlog:\n%s", outcome, f.log())
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The live base equals what configuration already resolves to, so nothing
	// is persisted; what this pins is that the retry succeeded and the restart
	// happened at all, instead of the stale read failing the run.
	if run.ObservedBaseBranch != nil || run.PRBaseBranch != nil {
		t.Fatalf("persisted bases = {observed %v, target %v}, want none for a base configuration already resolves to", run.ObservedBaseBranch, run.PRBaseBranch)
	}
	if run.HeadSHA == f.headSHA {
		t.Fatalf("recorded head = %s, want the repair commit recorded", run.HeadSHA)
	}
}

// applyRunPRBase is a write path for a forge-reported branch name, so it goes
// through the same validator every operator-facing writer runs: an unusable ref
// name is refused rather than persisted and re-read as a ref by every later
// step.
func TestApplyRunPRBase_RefusesAnUnusableBranchName(t *testing.T) {
	t.Parallel()

	f := newCIRepairFixture(t, true, nil)
	if err := applyRunPRBase(f.sctx, "bad..base"); err == nil || !strings.Contains(err.Error(), "not a usable branch name") {
		t.Fatalf("applyRunPRBase() error = %v, want the unusable name refused", err)
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ObservedBaseBranch != nil {
		t.Fatalf("observed base = %q, want nothing persisted", *run.ObservedBaseBranch)
	}
	if err := applyRunPRBase(f.sctx, "develop"); err != nil {
		t.Fatalf("applyRunPRBase() error = %v, want a usable name persisted", err)
	}
	run, err = f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ObservedBaseBranch == nil || *run.ObservedBaseBranch != "develop" {
		t.Fatalf("observed base = %v, want develop", run.ObservedBaseBranch)
	}
}

// The observation must never become target intent. runs.pr_base_branch is what
// the PR step retargets an existing pull request to and what bindExistingPR
// reads as an explicit request, so writing a forge-side observation there
// started a retarget war: adoption recorded layer2, the next stack merge
// retargeted the pull request to main, and the PR step moved it back to layer2.
// The observed base scopes the restarted Review instead, and the operator's own
// override is left exactly as it was.
func TestApplyRunPRBase_NeverWritesTargetIntent(t *testing.T) {
	t.Parallel()

	f := newCIRepairFixture(t, true, nil)
	operatorBase := "operator/base"
	f.sctx.Run.PRBaseBranch = &operatorBase

	if err := applyRunPRBase(f.sctx, "layer2"); err != nil {
		t.Fatalf("applyRunPRBase() error = %v", err)
	}
	if f.sctx.Run.PRBaseBranch == nil || *f.sctx.Run.PRBaseBranch != operatorBase {
		t.Fatalf("in-memory target base = %v, want the operator's %q untouched", f.sctx.Run.PRBaseBranch, operatorBase)
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRBaseBranch != nil {
		t.Fatalf("durable target base = %q, want the observation never persisted as target intent", *run.PRBaseBranch)
	}
	if run.ObservedBaseBranch == nil || *run.ObservedBaseBranch != "layer2" {
		t.Fatalf("observed base = %v, want layer2", run.ObservedBaseBranch)
	}
	if got := effectivePRBaseBranch(f.sctx); got != "layer2" {
		t.Fatalf("scoping base = %q, want the observed layer2 to supersede the operator base for scoping", got)
	}
	if got := targetPRBaseBranch(f.sctx); got != operatorBase {
		t.Fatalf("target base = %q, want the operator's %q: an observation never retargets", got, operatorBase)
	}
}

// The per-run base is an operator-style override: several consumers read a
// non-empty value as "the operator picked this base" and act on it - reverting
// a maintainer's forge-side retarget, and refusing a run whose persisted pull
// request has since closed. Aligning with the live base must therefore write it
// only when the live base actually differs from what configuration already
// resolves to, not merely from the (usually empty) override.
func TestApplyRunPRBase_OnlyMaterialisesADifferentBase(t *testing.T) {
	t.Parallel()

	f := newCIRepairFixture(t, true, nil)
	effective := effectivePRBaseBranch(f.sctx)
	if effective == "" {
		t.Fatalf("fixture has no effective base to compare against")
	}
	if err := applyRunPRBase(f.sctx, effective); err != nil {
		t.Fatalf("applyRunPRBase() error = %v", err)
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ObservedBaseBranch != nil || run.PRBaseBranch != nil {
		t.Fatalf("persisted bases = {observed %v, target %v}, want nothing for a base configuration already resolves to", run.ObservedBaseBranch, run.PRBaseBranch)
	}
	if err := applyRunPRBase(f.sctx, "develop"); err != nil {
		t.Fatalf("applyRunPRBase() error = %v", err)
	}
	run, err = f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ObservedBaseBranch == nil || *run.ObservedBaseBranch != "develop" {
		t.Fatalf("observed base = %v, want the retargeted base develop recorded", run.ObservedBaseBranch)
	}
}

// The restart is scoped by effectivePRBaseBranch, so that is the base the
// alignment has to compare against. A layer retargeted onto another parent must
// not keep scoping against the base configuration still resolves: the restarted
// Review then re-reviews every layer beneath a branch it no longer layers on.
func TestApplyRunPRBase_AlignsTheBaseTheRestartScopesBy(t *testing.T) {
	t.Parallel()

	f := newCIRepairFixture(t, true, nil)
	f.sctx.Config.PR.BaseBranch = "layer1"
	if got := effectivePRBaseBranch(f.sctx); got != "layer1" {
		t.Fatalf("scoping base = %q, want the configured layer1", got)
	}

	if err := applyRunPRBase(f.sctx, "layer2"); err != nil {
		t.Fatalf("applyRunPRBase() error = %v", err)
	}
	if got := effectivePRBaseBranch(f.sctx); got != "layer2" {
		t.Fatalf("scoping base = %q, want the restart scoped by the layer the PR now targets", got)
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ObservedBaseBranch == nil || *run.ObservedBaseBranch != "layer2" {
		t.Fatalf("observed base = %v, want layer2 recorded for the restart", run.ObservedBaseBranch)
	}
	if run.PRBaseBranch != nil {
		t.Fatalf("target base = %q, want it untouched by the observation", *run.PRBaseBranch)
	}

	// Equal to what the restart already scopes by: nothing to align.
	f.sctx.Run.ObservedBaseBranch = nil
	f.sctx.Config.PR.BaseBranch = ""
	if err := applyRunPRBase(f.sctx, effectivePRBaseBranch(f.sctx)); err != nil {
		t.Fatalf("applyRunPRBase() error = %v", err)
	}
	if f.sctx.Run.ObservedBaseBranch != nil {
		t.Fatalf("in-memory observed base = %q, want no write when the scoping base already agrees", *f.sctx.Run.ObservedBaseBranch)
	}
}

// The durable record is what every later pass reads, and the run worktree's
// head is one of the heads the run is credited with owning, so nothing may move
// a ref before that record lands. Moving the branch first left the branch - and
// therefore the worktree head - on a commit no Review approved while
// runs.head_sha still named the reviewed one, and the next CI poll then counted
// that foreign head as the run's own and could complete CI green on it.
func TestRecordRevalidationHead_FailedDurableWriteMovesNoRef(t *testing.T) {
	t.Parallel()

	f := newCIRepairFixture(t, true, nil)
	reviewed := f.headSHA
	adopted := gitCmd(t, f.dir, "commit-tree", gitCmd(t, f.dir, "rev-parse", "HEAD^{tree}"), "-p", reviewed, "-m", "rewritten outside the run")
	if err := f.sctx.DB.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := (&CIStep{}).recordRevalidationHead(f.sctx, adopted, "", false); err == nil {
		t.Fatal("recordRevalidationHead() error = nil, want the durable write failure surfaced")
	}
	if got := gitCmd(t, f.dir, "rev-parse", "refs/heads/feature"); got != reviewed {
		t.Fatalf("branch ref = %s, want the reviewed %s: the adopted head %s would then count as the run's own", got, reviewed, adopted)
	}
	if got := gitCmd(t, f.dir, "rev-parse", "HEAD"); got != reviewed {
		t.Fatalf("worktree head = %s, want the reviewed %s", got, reviewed)
	}
	if f.sctx.Run.HeadSHA != reviewed {
		t.Fatalf("in-memory run head = %s, want the reviewed %s", f.sctx.Run.HeadSHA, reviewed)
	}
}

type liveBaseTestHost struct {
	scm.Host
	readBase func() (string, error)
}

func (h *liveBaseTestHost) GetPRBaseBranch(context.Context, *scm.PR) (string, error) {
	return h.readBase()
}

func TestCIStep_RepairRefreshesTheBaseAfterTheFixer(t *testing.T) {
	t.Parallel()
	baseFile := filepath.Join(t.TempDir(), "live-base")
	if err := os.WriteFile(baseFile, []byte("layer1"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newCIRepairFixture(t, true, func(workDir string) {
		writeCIFix(workDir)
		if err := os.WriteFile(baseFile, []byte("main"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	gitCmd(t, f.dir, "branch", "layer1", "main")
	gitCmd(t, f.dir, "push", "origin", "layer1")
	initialBase := "layer1"
	f.sctx.Run.PRBaseBranch = &initialBase
	f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_PR_BASE_FILE="+baseFile)
	outcome, err := f.run(t)
	if err != nil || outcome == nil || outcome.RestartFrom != types.StepReview {
		t.Fatalf("repair = %#v, %v; want Review restart\n%s", outcome, err, f.log())
	}
	if effectivePRBaseBranch(f.sctx) != "main" {
		t.Fatalf("restart base = %s, want main", effectivePRBaseBranch(f.sctx))
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil || run.ObservedBaseBranch == nil || *run.ObservedBaseBranch != "main" {
		t.Fatalf("durable observed base = %+v, %v; want main", run, err)
	}
	if run.PRBaseBranch != nil {
		t.Fatalf("durable target base = %q, want the observation kept out of target intent", *run.PRBaseBranch)
	}
}

func TestCIStep_PluginBaseViolationPreventsAdoptionAndRepair(t *testing.T) {
	t.Parallel()
	for _, adoption := range []bool{false, true} {
		t.Run(map[bool]string{true: "adoption", false: "repair"}[adoption], func(t *testing.T) {
			called := false
			f := newCIRepairFixture(t, false, func(workDir string) { called = true; writeCIFix(workDir) })
			host, reason := buildHost(f.sctx, scm.ProviderGitHub)
			if host == nil {
				t.Fatal(reason)
			}
			liveHost := &liveBaseTestHost{Host: host, readBase: func() (string, error) { return "", plugin.ErrProtocol }}
			step := &CIStep{}
			var outcome *pipeline.StepOutcome
			var err error
			if adoption {
				target := gitCmd(t, f.dir, "commit-tree", gitCmd(t, f.dir, "rev-parse", "HEAD^{tree}"), "-p", f.headSHA, "-m", "rewrite")
				gitCmd(t, f.dir, "push", "origin", target+":refs/heads/feature")
				outcome, err = step.adoptPublishedHeadRewrite(f.sctx, liveHost, &scm.PR{Number: "42", URL: *f.sctx.Run.PRURL}, "provider head", target, true)
			} else {
				f.sctx.PreviousFindings = `{"findings":[{"id":"ci-1","severity":"error","description":"test failed","action":"auto-fix","category":"ci-check","check":"test"}]}`
				outcome, err = step.repairFromFindings(f.sctx, liveHost, &scm.PR{Number: "42", URL: *f.sctx.Run.PRURL})
			}
			if !errors.Is(err, plugin.ErrProtocol) || outcome != nil || called {
				t.Fatalf("outcome=%#v err=%v fixer called=%v; want protocol refusal before mutation", outcome, err, called)
			}
			run, readErr := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if run.HeadSHA != f.headSHA || run.ReviewApprovedHeadSHA == nil || f.localHead(t) != f.headSHA {
				t.Fatalf("protocol error changed run or worktree: %+v", run)
			}
		})
	}
}

func TestCIStep_CheckObserverCannotAdoptUnderUnknownPRState(t *testing.T) {
	t.Parallel()
	f := newCIRepairFixture(t, false, nil)
	step := &CIStep{}
	if outcome, skip, err := step.guardPublishedHead(f.sctx, nil, nil, false); err != nil || skip || outcome != nil {
		t.Fatalf("owned pre-check guard = %#v, %v, %v", outcome, skip, err)
	}
	target := gitCmd(t, f.dir, "commit-tree", gitCmd(t, f.dir, "rev-parse", "HEAD^{tree}"), "-p", f.headSHA, "-m", "rewrite during checks")
	gitCmd(t, f.dir, "push", "origin", target+":refs/heads/feature")
	if err := f.sctx.DB.SetRunCIReadyWithReason(f.sctx.Run.ID, true, false); err != nil {
		t.Fatal(err)
	}
	outcome, err := step.adoptPublishedHeadRewrite(f.sctx, nil, nil, "the pull request reports head", target, false)
	if err != nil || outcome != nil {
		t.Fatalf("adoption = %#v, %v", outcome, err)
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != f.headSHA || run.ReviewApprovedHeadSHA == nil || f.localHead(t) != f.headSHA || run.CIReadyAt != nil {
		t.Fatalf("unknown-state adoption changed validated state or kept readiness: %+v", run)
	}
}

func TestCIStep_UnverifiedPublishedPollClearsReadiness(t *testing.T) {
	t.Parallel()
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{true: "unreadable target", false: "unknown state with foreign target"}[missing], func(t *testing.T) {
			f := newCIRepairFixture(t, false, nil)
			if missing {
				gitCmd(t, f.dir, "push", "origin", "--delete", "feature")
			} else {
				target := gitCmd(t, f.dir, "commit-tree", gitCmd(t, f.dir, "rev-parse", "HEAD^{tree}"), "-p", f.headSHA, "-m", "rewrite")
				gitCmd(t, f.dir, "push", "origin", target+":refs/heads/feature")
			}
			if err := f.sctx.DB.SetRunCIReadyWithReason(f.sctx.Run.ID, true, false); err != nil {
				t.Fatal(err)
			}
			outcome, skip, err := (&CIStep{}).guardPublishedHead(f.sctx, nil, nil, false)
			if err != nil || !skip || outcome != nil {
				t.Fatalf("guard = %#v, %v, %v", outcome, skip, err)
			}
			run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil || run.CIReadyAt != nil {
				t.Fatalf("readiness survives unverified poll: %+v, %v", run, err)
			}
		})
	}
}

func TestCIStep_RevokedApprovalRestartsInsteadOfCompletingAMerge(t *testing.T) {
	t.Parallel()
	for _, fixing := range []bool{false, true} {
		t.Run(map[bool]string{true: "fix reentry", false: "poll reentry"}[fixing], func(t *testing.T) {
			f := newCIRepairFixture(t, false, nil)
			unpublished, err := f.sctx.DB.InsertRun(f.sctx.Repo.ID, f.sctx.Run.Branch, f.headSHA, f.sctx.Run.BaseSHA)
			if err != nil {
				t.Fatal(err)
			}
			unpublished.PRURL = f.sctx.Run.PRURL
			f.sctx.Run = unpublished
			review, err := f.sctx.DB.InsertStepResult(unpublished.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.sctx.DB.CompleteReviewStep(review.ID, unpublished.ID, f.headSHA, 0, 0, ""); err != nil {
				t.Fatal(err)
			}

			target := gitCmd(t, f.dir, "commit-tree", gitCmd(t, f.dir, "rev-parse", "HEAD^{tree}"), "-p", f.headSHA, "-m", "adopted head")
			step := &CIStep{}
			if _, err := step.recordRevalidationHead(f.sctx, target, "", false); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, f.dir, "reset", "--hard", target)
			f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_STATE=MERGED", "FAKE_CLI_PR_BASE=develop")
			f.sctx.Fixing = fixing
			resolved, err := step.ReconcileApprovalGate(f.sctx)
			if resolved || err != nil {
				t.Fatalf("reconcile = %v, %v; want parked", resolved, err)
			}
			outcome, err := step.Execute(f.sctx)
			if err != nil || outcome == nil || outcome.RestartFrom != types.StepReview {
				t.Fatalf("CI = %#v, %v; want Review restart", outcome, err)
			}
			if err := step.PrepareRestart(f.sctx, outcome.RestartFrom); err != nil {
				t.Fatal(err)
			}

			run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil || run.PRState != nil && *run.PRState == "merged" || run.ObservedBaseBranch == nil || *run.ObservedBaseBranch != "develop" {
				t.Fatalf("CI finalized or missed base alignment: %+v, %v", run, err)
			}
			if run.PRBaseBranch != nil {
				t.Fatalf("observed base became target intent: %q", *run.PRBaseBranch)
			}
		})
	}
}

func TestCIStep_AdoptionPreservesIgnoredTreeCollisions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, local, target string
		refuse              bool
		caseAlias           bool
	}{
		{"exact", "private.env", "private.env", true, false},
		{"local file blocks directory", "private", "private/file", true, false},
		{"local directory blocks file", "private/file", "private", true, false},
		{"leading whitespace", " private.env", " private.env", true, false},
		{"separate siblings", "private/cache/file", "private/src/file", false, false},
		{"prefix lookalike", "private.env", "private.env-example", false, false},
		{"filesystem case alias", "PRIVATE.env", "private.env", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCIRepairFixture(t, false, nil)
			if err := os.WriteFile(filepath.Join(f.dir, ".git", "info", "exclude"), []byte("private*\nPRIVATE*\n private*\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			targetPath := filepath.Join(f.dir, tc.target)
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(targetPath, []byte("published data"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, f.dir, "add", "-f", "--", tc.target)
			target := gitCmd(t, f.dir, "commit-tree", gitCmd(t, f.dir, "write-tree"), "-p", f.headSHA, "-m", "rewrite tracks ignored path")
			gitCmd(t, f.dir, "reset", "--hard", "HEAD")
			localPath := filepath.Join(f.dir, tc.local)
			if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(localPath, []byte("local secret"), 0o644); err != nil {
				t.Fatal(err)
			}
			refuse := tc.refuse
			if tc.caseAlias {
				_, lookupErr := os.Lstat(targetPath)
				refuse = lookupErr == nil
			}
			gitCmd(t, f.dir, "push", "origin", target+":refs/heads/feature")
			outcome, err := (&CIStep{}).Execute(f.sctx)
			if err != nil || outcome == nil || outcome.NeedsApproval != refuse {
				t.Fatalf("adoption = %#v, %v; refusal want %v", outcome, err, refuse)
			}
			if !refuse && outcome.RestartFrom != types.StepReview {
				t.Fatalf("noncolliding adoption did not restart: %#v", outcome)
			}
			data, err := os.ReadFile(localPath)
			if err != nil || string(data) != "local secret" {
				t.Fatalf("local content = %q, %v", data, err)
			}
			run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if refuse && (err != nil || run.HeadSHA != f.headSHA || run.ReviewApprovedHeadSHA == nil || f.localHead(t) != f.headSHA) {
				t.Fatalf("adoption mutated state: %+v, %v", run, err)
			}
		})
	}
}

func TestAdoptionPathCollisionRespectsCaseSensitivity(t *testing.T) {
	for _, tc := range []struct {
		local, target          string
		insensitive, collision bool
	}{
		{"PRIVATE.env", "private.env", true, true},
		{"PRIVATE", "private/file", true, true},
		{"PRIVATE/file", "private", true, true},
		{"private/SECRET", "PRIVATE/secret/file", true, true},
		{"private/SECRET/file", "PRIVATE/secret", true, true},
		{"PRIVATE.env", "private.env", false, false},
		{"PRIVATE", "private/file", false, false},
		{"PRIVATE/file", "private", false, false},
		{"PRIVATE/cache", "private/source", true, false},
		{"PRIVATE.env", "private.env-example", true, false},
	} {
		name := tc.local + ":" + tc.target
		if tc.insensitive {
			name += ":insensitive"
		} else {
			name += ":sensitive"
		}
		t.Run(name, func(t *testing.T) {
			got := adoptionPathCollision(tc.local+"\x00", tc.target+"\x00", tc.insensitive)
			if (got != "") != tc.collision || got != "" && got != tc.local {
				t.Fatalf("collision = %q, want collision %v preserving %q", got, tc.collision, tc.local)
			}
		})
	}
}

func TestCIStep_OwnershipReadinessClearFailureStopsTheStep(t *testing.T) {
	for _, mode := range []string{"unreadable target", "foreign target", "adoption", "pending Review"} {
		t.Run(mode, func(t *testing.T) {
			f := newCIRepairFixture(t, false, nil)
			if err := f.sctx.DB.SetRunCIReadyWithReason(f.sctx.Run.ID, true, false); err != nil {
				t.Fatal(err)
			}
			writeErr := errors.New("readiness write failed")
			f.sctx.SetCIReadiness = func(bool, bool) error { return writeErr }
			step := &CIStep{}
			var outcome *pipeline.StepOutcome
			var err error
			switch mode {
			case "unreadable target":
				gitCmd(t, f.dir, "push", "origin", "--delete", "feature")
				outcome, _, err = step.guardPublishedHead(f.sctx, nil, nil, false)
			case "foreign target":
				target := gitCmd(t, f.dir, "commit-tree", gitCmd(t, f.dir, "rev-parse", "HEAD^{tree}"), "-p", f.headSHA, "-m", "rewritten")
				gitCmd(t, f.dir, "push", "origin", target+":refs/heads/feature")
				outcome, _, err = step.guardPublishedHead(f.sctx, nil, nil, false)
			case "adoption":
				outcome, err = step.adoptPublishedHeadRewrite(f.sctx, nil, nil, "provider", "foreign", true)
			case "pending Review":
				review, insertErr := f.sctx.DB.InsertStepResult(f.sctx.Run.ID, types.StepReview)
				if insertErr != nil {
					t.Fatal(insertErr)
				}
				if err := f.sctx.DB.CompleteReviewStep(review.ID, f.sctx.Run.ID, f.headSHA, 0, 0, ""); err != nil {
					t.Fatal(err)
				}
				if err := f.sctx.DB.UpdateRunHeadSHAForRevalidation(f.sctx.Run.ID, f.headSHA); err != nil {
					t.Fatal(err)
				}
				outcome, err = step.Execute(f.sctx)
			}
			if !errors.Is(err, writeErr) || outcome != nil {
				t.Fatalf("outcome = %#v, err = %v; want the readiness persistence failure", outcome, err)
			}
			run, readErr := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if readErr != nil || run.CIReadyAt == nil || run.HeadSHA != f.headSHA || f.localHead(t) != f.headSHA {
				t.Fatalf("failed clear changed validation state: %+v, %v", run, readErr)
			}
		})
	}
}

func TestCIStep_ReadinessRecoversAfterUnverifiedPoll(t *testing.T) {
	for _, foreignChecks := range []bool{false, true} {
		name := "unreadable push target"
		if foreignChecks {
			name = "foreign provider head"
		}
		t.Run(name, func(t *testing.T) {
			f := newCIRepairFixture(t, false, nil)
			headFile := filepath.Join(t.TempDir(), "provider-head")
			writeHead := func(head string) {
				if err := os.WriteFile(headFile, []byte(head), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			writeHead(f.headSHA)
			f.sctx.Env = append(f.sctx.Env, "FAKE_CLI_PR_HEAD_FILE="+headFile,
				`FAKE_CLI_CHECKS=[{"name":"test","state":"SUCCESS","bucket":"pass"}]`)
			stop := errors.New("three polls observed")
			polls := 0
			step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
				polls++
				run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if ready := run.CIReadyAt != nil; ready != (polls != 2) {
					t.Fatalf("poll %d readiness = %v; want green, withdrawn, green", polls, ready)
				}
				switch polls {
				case 1:
					if foreignChecks {
						writeHead(strings.Repeat("b", 40))
					} else {
						gitCmd(t, f.dir, "push", "origin", ":refs/heads/feature")
					}
				case 2:
					if foreignChecks {
						writeHead(f.headSHA)
					} else {
						gitCmd(t, f.dir, "push", "origin", f.headSHA+":refs/heads/feature")
					}
				case 3:
					return stop
				}
				return nil
			}}
			if _, err := step.Execute(f.sctx); !errors.Is(err, stop) {
				t.Fatalf("monitor = %v after %d polls; log:\n%s", err, polls, f.log())
			}
		})
	}
}

func TestCIStep_AdoptionFetchCancellationStopsTransport(t *testing.T) {
	f := newCIRepairFixture(t, false, nil)
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	started := filepath.Join(t.TempDir(), "fetch-started")
	f.sctx.Env = fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":          "git-stalled-fetch",
		"FAKE_CLI_FETCH_STARTED": started,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.sctx.Ctx = ctx
	done := make(chan error, 1)
	go func() {
		_, err := fetchVerifiedPublishedHead(f.sctx)
		done <- err
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
waiting:
	for {
		select {
		case <-tick.C:
			if _, err := os.Stat(started); err == nil {
				break waiting
			}
		case err := <-done:
			t.Fatalf("fetch exited before starting its transport: %v", err)
		case <-deadline.C:
			t.Fatal("fetch never started its transport")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled fetch succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("transport kept fetch pipes open after cancellation")
	}
	if f.localHead(t) != f.headSHA || f.remoteHead(t) != f.headSHA {
		t.Fatal("cancelled fetch moved the local or published head")
	}
}

func TestCIStep_RevokedApprovalDuringPollingRestartsReview(t *testing.T) {
	f := newCIRepairFixture(t, false, nil)
	review, err := f.sctx.DB.InsertStepResult(f.sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.sctx.DB.CompleteReviewStep(review.ID, f.sctx.Run.ID, f.headSHA, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	f.sctx.Env = append(f.sctx.Env, `FAKE_CLI_CHECKS=[{"name":"test","state":"SUCCESS","bucket":"pass"}]`)
	polls := 0
	step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
		polls++
		if polls > 1 {
			return errors.New("monitor continued without review authority")
		}
		return f.sctx.DB.UpdateRunHeadSHAForRevalidation(f.sctx.Run.ID, strings.Repeat("b", 40))
	}}
	outcome, err := step.Execute(f.sctx)
	if err != nil || outcome == nil || outcome.RestartFrom != types.StepReview {
		t.Fatalf("CI = %#v, %v; want Review restart", outcome, err)
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil || run.CIReadyAt != nil {
		t.Fatalf("revoked Review retained readiness: %+v, %v", run, err)
	}
}

func TestCIStep_TerminalPRStateRequiresReviewAuthority(t *testing.T) {
	for _, state := range []string{"merged", "closed"} {
		t.Run(state, func(t *testing.T) {
			f := newCIRepairFixture(t, false, nil)
			review, err := f.sctx.DB.InsertStepResult(f.sctx.Run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.sctx.DB.CompleteReviewStep(review.ID, f.sctx.Run.ID, f.headSHA, 0, 0, ""); err != nil {
				t.Fatal(err)
			}
			if err := f.sctx.DB.UpdateRunHeadSHAForRevalidation(f.sctx.Run.ID, strings.Repeat("b", 40)); err != nil {
				t.Fatal(err)
			}
			called := false
			f.sctx.SetPRState = func(string) error { called = true; return nil }
			if err := setCIMonitorPRState(f.sctx, state); err == nil || called {
				t.Fatalf("terminal PR state accepted without Review: callback=%v, error=%v", called, err)
			}
			run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil || run.Status.Terminal() || run.PRState != nil && (*run.PRState == "merged" || *run.PRState == "closed") {
				t.Fatalf("terminal state was persisted: %+v, %v", run, err)
			}
		})
	}
}

// A provider that cannot report the pull request's live base is refused loudly
// at a restart at Review rather than restarted against the base the run already
// had: after a forge-side retarget that base can be the parent branch the layer
// no longer layers on, so the restart would re-review every layer beneath it or
// fail outright when the forge deleted that branch.
func TestPrepareRestart_RefusesAHostWithoutLiveBase(t *testing.T) {
	t.Parallel()

	f := newCIRepairFixture(t, true, nil)
	// Forgejo is a provider without a live-base reader; the fixture's repository
	// only has to resolve to it for buildHost to build that host.
	f.sctx.Repo.UpstreamURL = "https://codeberg.org/octo/widgets.git"
	f.sctx.Config.ForgejoAXIPath = "forgejo-axi"

	err := (&CIStep{}).PrepareRestart(f.sctx, types.StepReview)
	if !errors.Is(err, errLivePRBaseUnsupported) {
		t.Fatalf("PrepareRestart() error = %v, want the unreadable live base refused", err)
	}
	if !strings.Contains(err.Error(), "start a new run with --base-branch") {
		t.Fatalf("PrepareRestart() error = %v, want actionable guidance", err)
	}
	run, dbErr := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	if run.ObservedBaseBranch != nil || run.PRBaseBranch != nil {
		t.Fatalf("refused restart changed a base: observed=%v target=%v", run.ObservedBaseBranch, run.PRBaseBranch)
	}

	// A restart that is not at Review scopes nothing, so it is not refused.
	if err := (&CIStep{}).PrepareRestart(f.sctx, types.StepTest); err != nil {
		t.Fatalf("PrepareRestart(Test) error = %v, want no alignment needed there", err)
	}
}
