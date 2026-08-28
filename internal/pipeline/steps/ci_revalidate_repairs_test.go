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
// review authority.
type ciRepairFixture struct {
	sctx     *pipeline.StepContext
	dir      string
	upstream string
	headSHA  string
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
	return &ciRepairFixture{sctx: sctx, dir: dir, upstream: upstream, headSHA: headSHA, logs: logs}
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
			f := newCIRepairFixture(t, tc.revalidate, writeCIFix)
			outcome, err := f.run(t)
			if err != nil {
				t.Fatalf("CI step returned error: %v\nlog:\n%s", err, f.log())
			}

			gotRestart := outcome != nil && outcome.RestartFrom == types.StepReview
			if gotRestart != tc.wantRestart {
				t.Errorf("RestartFrom review = %v, want %v (outcome %#v)", gotRestart, tc.wantRestart, outcome)
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
			// The step states which policy is in force before it does anything.
			if !strings.Contains(f.log(), "CI repair policy:") {
				t.Errorf("CI step did not report its repair policy; log:\n%s", f.log())
			}
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
			outcome, err := f.run(t)
			if err != nil {
				t.Fatalf("CI step returned error: %v", err)
			}
			if outcome != nil && outcome.RestartFrom != "" {
				t.Errorf("a no-change repair requested a restart: %#v", outcome)
			}
			if f.localHead(t) != f.headSHA {
				t.Error("a no-change repair created a commit")
			}
			if f.remoteHead(t) != f.headSHA {
				t.Error("a no-change repair published something")
			}
			if !strings.Contains(f.log(), "CI fix produced no changes") {
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
			t.Parallel()
			var f *ciRepairFixture
			f = newCIRepairFixture(t, tc.revalidate, func(workDir string) {
				os.WriteFile(filepath.Join(workDir, "resolved.txt"), []byte("resolved"), 0o644)
				gitCmd(t, workDir, "add", "-A")
				gitCmd(t, workDir, "commit", "-m", "agent resolved the failure")
			})
			outcome, err := f.run(t)
			if err != nil {
				t.Fatalf("CI step returned error: %v\nlog:\n%s", err, f.log())
			}
			if f.localHead(t) == f.headSHA {
				t.Fatal("the agent's own commit was not detected")
			}
			gotRestart := outcome != nil && outcome.RestartFrom == types.StepReview
			if gotRestart != tc.wantRestart {
				t.Errorf("RestartFrom review = %v, want %v", gotRestart, tc.wantRestart)
			}
			if moved := f.remoteHead(t) != f.headSHA; moved != tc.wantRemoteMoved {
				t.Errorf("remote advanced = %v, want %v", moved, tc.wantRemoteMoved)
			}
		})
	}
}

// Once a repair has reached the remote and its push binding is durable, a
// later gate-mirror synchronization failure must not turn that successful
// publication into a failed fix attempt. The mirror can be repaired by a
// later Push step; the CI monitor must keep watching the published commit.
func TestCIStep_PublishedRepairSurvivesLateGateMirrorFailure(t *testing.T) {
	f := newCIRepairFixture(t, false, nil)
	writeCIFix(f.dir)
	f.sctx.GateDir = filepath.Join(t.TempDir(), "invalid-gate")
	if err := os.MkdirAll(f.sctx.GateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	changed, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check")
	if err != nil {
		t.Fatalf("published repair was misreported as failed: %v", err)
	}
	if !changed {
		t.Fatal("published repair was not reported as a real change")
	}
	publishedHead := f.localHead(t)
	if publishedHead == f.headSHA || f.remoteHead(t) != publishedHead {
		t.Fatalf("repair was not published: prior=%s local=%s remote=%s", f.headSHA, publishedHead, f.remoteHead(t))
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != publishedHead || run.LastPushedSHA == nil || *run.LastPushedSHA != publishedHead {
		t.Fatalf("published repair was not durably bound to %s: %#v", publishedHead, run)
	}
	if !strings.Contains(f.log(), "CI repair was published, but gate mirror synchronization failed") {
		t.Fatalf("late gate-mirror failure was not logged as a warning:\n%s", f.log())
	}
	if !strings.Contains(f.log(), "committed and pushed CI repair") {
		t.Fatalf("successful repair publication was not logged:\n%s", f.log())
	}
}

func TestCIStep_PublishPolicyAllowsConflictRebaseContinuity(t *testing.T) {
	f := newCIRepairFixture(t, false, nil)
	gitCmd(t, f.dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(f.dir, "base-advance.txt"), []byte("advanced\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "advance base")
	rewriteBase := gitCmd(t, f.dir, "rev-parse", "HEAD")
	gitCmd(t, f.dir, "checkout", "feature")
	gitCmd(t, f.dir, "rebase", "main")
	rebasedHead := gitCmd(t, f.dir, "rev-parse", "HEAD")
	if rebasedHead == f.headSHA {
		t.Fatal("rebase did not rewrite the reviewed head")
	}

	changed, err := (&CIStep{}).commitRepairWithRewriteBase(f.sctx, "resolve merge conflict", rewriteBase)
	if err != nil {
		t.Fatalf("publish rebased repair: %v", err)
	}
	if !changed || f.remoteHead(t) != rebasedHead {
		t.Fatalf("rebased repair was not published: changed=%v remote=%s want=%s", changed, f.remoteHead(t), rebasedHead)
	}

	if err := os.WriteFile(filepath.Join(f.dir, "follow-up.txt"), []byte("follow-up\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err = (&CIStep{}).commitRepair(f.sctx, "finish CI repair")
	if err != nil {
		t.Fatalf("publish descendant after rebased repair: %v", err)
	}
	if !changed || f.remoteHead(t) != f.localHead(t) {
		t.Fatalf("follow-up repair was not published: changed=%v remote=%s local=%s", changed, f.remoteHead(t), f.localHead(t))
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
			t.Parallel()
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

// The published repair still goes through the review-approved-head guard, so a
// run whose review authority was never recorded cannot publish one. This is
// what keeps the default path a guarded publication rather than a bare push.
func TestCIStep_PublishedRepairStillRequiresReviewAuthority(t *testing.T) {
	t.Parallel()
	f := newCIRepairFixture(t, false, writeCIFix)
	if err := f.sctx.DB.UpdateRunReviewApprovedHeadSHA(f.sctx.Run.ID, ""); err != nil {
		t.Fatal(err)
	}
	f.sctx.Run.ReviewApprovedHeadSHA = nil

	if _, err := f.run(t); err != nil {
		t.Fatalf("CI step returned error: %v", err)
	}
	if f.remoteHead(t) != f.headSHA {
		t.Fatal("a repair was published without a recorded review-approved head")
	}
	if !strings.Contains(f.log(), "review-approved head") {
		t.Errorf("expected the refusal to name the missing review authority; log:\n%s", f.log())
	}
}
