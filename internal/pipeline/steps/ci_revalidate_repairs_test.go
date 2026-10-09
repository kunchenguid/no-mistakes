package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
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

const boundCIRevalidationFindings = `{"findings":[{"id":"ci-1","severity":"error","description":"repair CI","action":"ask-user","category":"ci-check","check":"test"}]}`

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
		"FAKE_CLI_PR_HEAD_SHA="+headSHA,
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

// An explicit response-level demand is tighter than the executor's retained
// false policy. This is the active published-run recovery case: a descendant
// CI repair would ordinarily publish immediately, but this one must remain
// local and revoke review authority until the same run revalidates from Review.
func TestCIStep_ResponseDemandTightensFalseRepairPolicy(t *testing.T) {
	f := newCIRepairFixture(t, false, nil)
	f.sctx.RequireReviewRevalidation = true
	writeCIFix(f.dir)

	repair, err := (&CIStep{}).commitRepair(f.sctx, "repair the failing check", nil)
	if err != nil {
		t.Fatalf("CI repair returned error: %v\nlog:\n%s", err, f.log())
	}
	if !repair.HeadAdvanced || !repair.Revalidate {
		t.Fatalf("repair = %#v, want a held repair requiring Review", repair)
	}
	localHead := f.localHead(t)
	if got := f.remoteHead(t); got != f.headSHA {
		t.Fatalf("remote moved to %s before revalidation; want %s", got, f.headSHA)
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ReviewApprovedHeadSHA != nil {
		t.Fatalf("review approval survived explicit revalidation demand: %s", *run.ReviewApprovedHeadSHA)
	}
	if run.HeadSHA != localHead {
		t.Fatalf("current local pipeline head = %s, want held repair %s", run.HeadSHA, localHead)
	}
	if run.LastPushedSHA == nil || *run.LastPushedSHA != f.headSHA {
		t.Fatalf("historical published head = %v, want prior head %s retained until fresh validation", run.LastPushedSHA, f.headSHA)
	}
}

func TestPipeline_BoundCIRepairPublishesOnlyAfterEveryValidatorObservesTheRepairedHead(t *testing.T) {
	dir, baseSHA, initialHead := setupGitRepo(t)
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", "origin", "feature")
	gitCmd(t, dir, "checkout", "--detach", initialHead)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, initialHead, config.Commands{})
	sctx.Config.CI.RevalidateRepairs = false
	prURL := "https://github.com/test/repo/pull/42"
	if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, prURL); err != nil {
		t.Fatal(err)
	}
	sctx.Run.PRURL = &prURL
	env, _ := fakeGH(t, prURL)
	for key, value := range environmentEntries(t, env) {
		t.Setenv(key, value)
	}

	repairedHead := ""
	assertRepairedValidation := func(name types.StepName, sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		if repairedHead == "" {
			if name == types.StepReview {
				return &pipeline.StepOutcome{ReviewApprovedHeadSHA: sctx.Run.HeadSHA}, nil
			}
			return &pipeline.StepOutcome{}, nil
		}
		local := gitCmd(t, dir, "rev-parse", "HEAD")
		remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
		if sctx.Run.HeadSHA != repairedHead || local != repairedHead {
			return nil, fmt.Errorf("%s validated run=%s local=%s, want repaired head %s", name, sctx.Run.HeadSHA, local, repairedHead)
		}
		if remote != initialHead {
			return nil, fmt.Errorf("%s observed premature publication %s, want prior head %s", name, remote, initialHead)
		}
		outcome := &pipeline.StepOutcome{}
		if name == types.StepReview {
			outcome.ReviewApprovedHeadSHA = repairedHead
		}
		return outcome, nil
	}

	ciCalls := 0
	ci := &transitionStep{name: types.StepCI, execute: func(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		ciCalls++
		switch ciCalls {
		case 1:
			return &pipeline.StepOutcome{NeedsApproval: true, Findings: boundCIRevalidationFindings}, nil
		case 2:
			if !ctx.Fixing || !ctx.RequireReviewRevalidation {
				return nil, fmt.Errorf("bound repair context = fixing:%v require_review:%v", ctx.Fixing, ctx.RequireReviewRevalidation)
			}
			if err := os.WriteFile(filepath.Join(dir, "bound-ci-fix.txt"), []byte("repaired\n"), 0o644); err != nil {
				return nil, err
			}
			repair, err := (&CIStep{}).commitRepair(ctx, "apply bound CI repair", nil)
			if err != nil {
				return nil, err
			}
			if !repair.HeadAdvanced || !repair.Revalidate {
				return nil, fmt.Errorf("bound CI repair = %+v, want held head and Review restart", repair)
			}
			repairedHead = ctx.Run.HeadSHA
			if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != initialHead {
				return nil, fmt.Errorf("bound repair published %s before validation; want %s", remote, initialHead)
			}
			return &pipeline.StepOutcome{RestartFrom: types.StepReview}, nil
		default:
			if ctx.Fixing || ctx.RequireReviewRevalidation || ctx.Run.HeadSHA != repairedHead {
				return nil, fmt.Errorf("final CI context = fixing:%v require_review:%v head:%s want:%s", ctx.Fixing, ctx.RequireReviewRevalidation, ctx.Run.HeadSHA, repairedHead)
			}
			if remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); remote != repairedHead {
				return nil, fmt.Errorf("final CI remote = %s, want repaired head %s", remote, repairedHead)
			}
			return &pipeline.StepOutcome{}, nil
		}
	}}
	steps := []pipeline.Step{
		&transitionStep{name: types.StepReview, execute: func(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
			return assertRepairedValidation(types.StepReview, sctx)
		}},
		&transitionStep{name: types.StepTest, execute: func(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
			return assertRepairedValidation(types.StepTest, sctx)
		}},
		&transitionStep{name: types.StepDocument, execute: func(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
			return assertRepairedValidation(types.StepDocument, sctx)
		}},
		&transitionStep{name: types.StepLint, execute: func(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
			return assertRepairedValidation(types.StepLint, sctx)
		}},
		&PushStep{},
		&transitionStep{name: types.StepPR, execute: func(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
			return &pipeline.StepOutcome{}, nil
		}},
		ci,
	}
	appPaths := paths.WithRoot(t.TempDir())
	if err := appPaths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	exec := pipeline.NewExecutor(sctx.DB, appPaths, sctx.Config, ag, steps, nil)
	exec.SetForgeContext(&forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"})
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), sctx.Run, sctx.Repo, dir) }()

	var ciResult *db.StepResult
	deadline := time.Now().Add(10 * time.Second)
	for ciResult == nil {
		results, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, result := range results {
			if result.StepName == types.StepCI && result.Status == types.StepStatusAwaitingApproval {
				ciResult = result
				break
			}
		}
		if ciResult == nil {
			select {
			case err := <-done:
				t.Fatalf("pipeline returned before CI gate: %v", err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("pipeline did not park at its initial CI gate")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	rounds, err := sctx.DB.GetRoundsByStep(ciResult.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("CI gate rounds = %#v err=%v", rounds, err)
	}
	binding := pipeline.ResponseRevalidationBinding{
		OperationID: "operation-real-git", Fingerprint: "sha256:real-git", RunID: sctx.Run.ID,
		RepoID: sctx.Repo.ID, Branch: sctx.Run.Branch, HeadSHA: initialHead, StepResultID: ciResult.ID, RoundID: rounds[0].ID,
	}
	if _, operation, replayed, err := exec.RespondWithBoundRevalidation(types.StepCI, types.ActionFix, []string{"ci-1"}, nil, nil, nil, "", binding); err != nil || operation == nil || replayed {
		t.Fatalf("bound response: operation=%#v replayed=%v err=%v", operation, replayed, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pipeline: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("pipeline did not finish bound repair revalidation")
	}
	if repairedHead == "" || repairedHead == initialHead {
		t.Fatalf("repaired head = %q, initial = %q", repairedHead, initialHead)
	}
	remote := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if remote != repairedHead {
		t.Fatalf("final remote = %s, want repaired head %s", remote, repairedHead)
	}
	refreshed, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || refreshed.LastPushedSHA == nil || *refreshed.LastPushedSHA != repairedHead {
		t.Fatalf("published receipt = %#v err=%v, want %s", refreshed, err, repairedHead)
	}
}

func TestPipeline_BoundCIProtectedPathRetryCommitsOnlyTheResolvedRetainedRepair(t *testing.T) {
	f := newCIRepairFixture(t, false, nil)
	gitCmd(t, f.dir, "checkout", "--detach", f.headSHA)
	f.sctx.Config.ProtectedPaths = []string{"*.lock"}
	if err := f.sctx.DB.UpdateRunPRURL(f.sctx.Run.ID, *f.sctx.Run.PRURL); err != nil {
		t.Fatal(err)
	}
	for key, value := range environmentEntries(t, f.sctx.Env) {
		t.Setenv(key, value)
	}

	agentCalls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		agentCalls++
		if err := os.WriteFile(filepath.Join(f.dir, "allowed-fix.go"), []byte("retained allowed repair\n"), 0o644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(f.dir, "package.lock"), []byte("protected repair\n"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"summary":"repair CI","code_change_needed":true}`)}, nil
	}}

	var order []types.StepName
	pass := func(name types.StepName) pipeline.Step {
		return &transitionStep{name: name, execute: func(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
			order = append(order, name)
			out := &pipeline.StepOutcome{}
			if name == types.StepReview {
				out.ReviewApprovedHeadSHA = sctx.Run.HeadSHA
			}
			return out, nil
		}}
	}
	push := &transitionStep{name: types.StepPush, execute: func(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		order = append(order, types.StepPush)
		return (&PushStep{}).Execute(sctx)
	}}
	realCI := &CIStep{}
	ciCalls := 0
	ci := &transitionStep{name: types.StepCI, execute: func(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		order = append(order, types.StepCI)
		ciCalls++
		switch ciCalls {
		case 1:
			return &pipeline.StepOutcome{NeedsApproval: true, Findings: boundCIRevalidationFindings}, nil
		case 2, 3, 4:
			return realCI.Execute(sctx)
		default:
			return &pipeline.StepOutcome{}, nil
		}
	}}
	steps := []pipeline.Step{
		pass(types.StepReview), pass(types.StepTest), pass(types.StepDocument), pass(types.StepLint),
		push, pass(types.StepPR), ci,
	}
	appPaths := paths.WithRoot(t.TempDir())
	if err := appPaths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	exec := pipeline.NewExecutor(f.sctx.DB, appPaths, f.sctx.Config, ag, steps, nil)
	exec.SetForgeContext(&forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"})
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), f.sctx.Run, f.sctx.Repo, f.dir) }()

	waitGate := func(roundCount int) (*types.Findings, string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			results, err := f.sctx.DB.GetStepsByRun(f.sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, result := range results {
				if result.StepName != types.StepCI || result.FindingsJSON == nil {
					continue
				}
				rounds, err := f.sctx.DB.GetRoundsByStep(result.ID)
				if err == nil && len(rounds) >= roundCount && (result.Status == types.StepStatusAwaitingApproval || result.Status == types.StepStatusFixReview) {
					findings, err := types.ParseFindingsJSON(*result.FindingsJSON)
					if err != nil {
						t.Fatal(err)
					}
					return &findings, result.ID
				}
			}
			select {
			case err := <-done:
				t.Fatalf("pipeline returned before CI gate %d: %v", roundCount, err)
			default:
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for CI gate %d", roundCount)
		return nil, ""
	}
	respondToProtectedRefusal := func(findings *types.Findings) {
		t.Helper()
		var selected, ignored []string
		for _, finding := range findings.Items {
			switch {
			case finding.ID == "protected-path-refusal":
				selected = append(selected, finding.ID)
			case finding.ID != "" && finding.ID != "ci-1":
				ignored = append(ignored, finding.ID)
			}
		}
		if !slices.Equal(selected, []string{"protected-path-refusal"}) {
			t.Fatalf("protected-path finding missing from %+v", findings.Items)
		}
		if _, err := exec.RespondWithOverrides(types.StepCI, types.ActionFix, selected, ignored, nil, nil, ""); err != nil {
			t.Fatalf("protected-path retry: %v", err)
		}
	}

	_, ciResultID := waitGate(1)
	rounds, err := f.sctx.DB.GetRoundsByStep(ciResultID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("initial rounds=%#v err=%v", rounds, err)
	}
	binding := pipeline.ResponseRevalidationBinding{
		OperationID: "operation-bound-protected", Fingerprint: "sha256:bound-protected", RunID: f.sctx.Run.ID,
		RepoID: f.sctx.Repo.ID, Branch: f.sctx.Run.Branch, HeadSHA: f.headSHA, StepResultID: ciResultID, RoundID: rounds[0].ID,
	}
	if _, operation, replayed, err := exec.RespondWithBoundRevalidation(types.StepCI, types.ActionFix, []string{"ci-1"}, nil, nil, nil, "", binding); err != nil || operation == nil || replayed {
		t.Fatalf("bound response operation=%#v replayed=%v err=%v", operation, replayed, err)
	}

	firstRefusal, _ := waitGate(2)
	if agentCalls != 1 || ciCalls != 2 {
		t.Fatalf("initial retained repair executions: agent=%d ci=%d", agentCalls, ciCalls)
	}
	respondToProtectedRefusal(firstRefusal)
	secondRefusal, _ := waitGate(3)
	if agentCalls != 1 || ciCalls != 3 {
		t.Fatalf("unresolved retry reran producer or skipped CI: agent=%d ci=%d", agentCalls, ciCalls)
	}
	if got := gitStatusPorcelain(t, f.dir); !strings.Contains(got, "allowed-fix.go") || !strings.Contains(got, "package.lock") {
		t.Fatalf("unresolved protected retry did not retain both changes: %q", got)
	}

	if err := os.Remove(filepath.Join(f.dir, "package.lock")); err != nil {
		t.Fatal(err)
	}
	if got := gitStatusPorcelain(t, f.dir); !strings.Contains(got, "allowed-fix.go") || strings.Contains(got, "package.lock") {
		t.Fatalf("operator resolution status=%q", got)
	}
	respondToProtectedRefusal(secondRefusal)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pipeline: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("pipeline did not finish the retained bound repair")
	}

	if ciCalls != 5 || agentCalls != 1 {
		t.Fatalf("completed executions: agent=%d ci=%d, want one producer and five CI entries", agentCalls, ciCalls)
	}
	if got := gitStatusPorcelain(t, f.dir); got != "" {
		t.Fatalf("completed retained repair left a dirty worktree: %q", got)
	}
	repairedHead := gitCmd(t, f.dir, "rev-parse", "HEAD")
	if repairedHead == f.headSHA {
		t.Fatal("resolved retained repair did not advance HEAD")
	}
	if got := gitCmd(t, f.dir, "show", "HEAD:allowed-fix.go"); got != "retained allowed repair" {
		t.Fatalf("allowed retained repair = %q", got)
	}
	if got := gitCmd(t, f.dir, "ls-tree", "--name-only", "HEAD", "--", "package.lock"); got != "" {
		t.Fatalf("protected path reached the repair commit: %q", got)
	}
	if got := f.remoteHead(t); got != repairedHead {
		t.Fatalf("remote head = %s, want revalidated retained repair %s", got, repairedHead)
	}
	wantOrder := []types.StepName{
		types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI,
		types.StepCI, types.StepCI, types.StepCI,
		types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI,
	}
	if !slices.Equal(order, wantOrder) {
		t.Fatalf("pipeline order = %v, want %v", order, wantOrder)
	}
}

func TestCIRepairPolicyLogSeparatesConfigurationFromResponseDemand(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured bool
		response   bool
		effective  bool
	}{
		{name: "ordinary publish policy"},
		{name: "configured revalidation", configured: true, effective: true},
		{name: "response tightens false configuration", response: true, effective: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx := &pipeline.StepContext{Config: &config.Config{}, RequireReviewRevalidation: tc.response}
			sctx.Config.CI.RevalidateRepairs = tc.configured
			got := ciRepairPolicyLog(sctx)
			for _, want := range []string{
				"ci.revalidate_repairs: " + strconv.FormatBool(tc.configured),
				"response_requires_review_revalidation: " + strconv.FormatBool(tc.response),
				"effective_revalidation: " + strconv.FormatBool(tc.effective),
			} {
				if !strings.Contains(got, want) {
					t.Errorf("policy log %q does not contain %q", got, want)
				}
			}
		})
	}
}

// A repair the agent declined to make is not a repair under either policy: no
// commit, no publication, no restart, and the attempt budget still decides
// when to stop.
func TestCIStep_NoChangeRepairNeitherPublishesNorRestarts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		policy   bool
		response bool
	}{
		{name: "publish_policy"},
		{name: "revalidate_policy", policy: true},
		{name: "response_demand", response: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCIRepairFixture(t, tc.policy, nil)
			f.sctx.RequireReviewRevalidation = tc.response
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
