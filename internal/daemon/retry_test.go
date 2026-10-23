package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/verificationplan"
)

// correctingReviewStep commits one correction on its first execution, the way
// a review fix round does, and then asks its agent for a verdict.
type correctingReviewStep struct {
	mu    sync.Mutex
	heads []string
}

func (s *correctingReviewStep) Name() types.StepName { return types.StepReview }

func (s *correctingReviewStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.mu.Lock()
	s.heads = append(s.heads, sctx.Run.HeadSHA)
	first := len(s.heads) == 1
	s.mu.Unlock()
	if first {
		if err := os.WriteFile(filepath.Join(sctx.WorkDir, "fix.txt"), []byte("corrected"), 0o644); err != nil {
			return nil, err
		}
		for _, args := range [][]string{{"add", "fix.txt"}, {"commit", "-m", "review fix"}} {
			if _, err := git.Run(sctx.Ctx, sctx.WorkDir, args...); err != nil {
				return nil, err
			}
		}
		head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
		if err != nil {
			return nil, err
		}
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, head); err != nil {
			return nil, err
		}
		sctx.Run.HeadSHA = head
	}
	if _, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "review", CWD: sctx.WorkDir}); err != nil {
		return nil, fmt.Errorf("review agent: %w", err)
	}
	return &pipeline.StepOutcome{}, nil
}

func (s *correctingReviewStep) seenHeads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.heads...)
}

// writeProviderOutageClaude writes a mock claude that fails like a provider
// outage while flag exists and answers cleanly once it is removed.
func writeProviderOutageClaude(t *testing.T, p *paths.Paths, flag string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nif [ -e " + shellQuoteForTest(flag) + " ]; then echo 'provider processing error' >&2; exit 1; fi\n" +
		`printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"findings":[],"summary":"clean"}}'` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: "+path+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRetryContinuesAFailedReviewFromThePreservedHead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the provider-outage agent is a POSIX shell script")
	}
	rebase := &mockPassStep{name: types.StepRebase}
	review := &correctingReviewStep{}
	test := &mockPassStep{name: types.StepTest}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{rebase, review, test} })
	repo, submitted := setupTestGitRepo(t, p, d, "retry-repo")
	flag := filepath.Join(t.TempDir(), "provider-down")
	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	writeProviderOutageClaude(t, p, flag)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var first ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", New: submitted, Old: submitted, Intent: "keep the corrections",
	}, &first); err != nil {
		t.Fatal(err)
	}
	failed := waitForRunTerminalState(t, d, first.RunID)
	failedSteps, err := d.GetStepsByRun(failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != types.RunFailed || failedSteps[1].FailureCause == nil || *failedSteps[1].FailureCause != db.StepFailureCauseAgentInvocation {
		t.Fatalf("first run = %s, review = %+v; want a review failed on an agent invocation error", failed.Status, failedSteps[1])
	}
	preserved := failed.HeadSHA
	if preserved == submitted {
		t.Fatal("the review's correction did not move the run head")
	}

	gitCmd(t, repo.WorkingPath, "fetch", "gate", custody.RecoveryRef(failed.ID))
	gitCmd(t, repo.WorkingPath, "merge", "--ff-only", "FETCH_HEAD")
	if err := d.SetRunCustodyReturned(failed.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	var result ipc.RetryRunResult
	if err := client.Call(ipc.MethodRetryRun, &ipc.RetryRunParams{RepoID: repo.ID, Branch: "main", RunID: failed.ID, CallerHeadSHA: preserved}, &result); err != nil {
		t.Fatalf("retry: %v", err)
	}
	retried := waitForRunTerminalState(t, d, result.RunID)
	if retried.Status != types.RunCompleted || retried.SubmittedHeadSHA == nil || *retried.SubmittedHeadSHA != preserved {
		t.Fatalf("retried run = %+v, want completed from preserved head %s", retried, preserved)
	}
	if retried.Intent == nil || *retried.Intent != *failed.Intent || *retried.IntentSource != *failed.IntentSource {
		t.Fatalf("retried run intent = %v/%v, want the failed run's %v/%v", retried.Intent, retried.IntentSource, *failed.Intent, *failed.IntentSource)
	}
	if got := rebase.execCnt.Load(); got != 1 {
		t.Errorf("rebase executed %d times, want once: the retry carries it", got)
	}
	if got := test.execCnt.Load(); got != 1 {
		t.Errorf("test executed %d times, want once, after the retried review", got)
	}
	if heads := review.seenHeads(); len(heads) != 2 || heads[0] != submitted || heads[1] != preserved {
		t.Errorf("review saw heads %v, want %s then a fresh review of %s", heads, submitted, preserved)
	}
	retriedSteps, err := d.GetStepsByRun(retried.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retriedSteps[0].CarriedFrom == nil || *retriedSteps[0].CarriedFrom != failedSteps[0].ID {
		t.Errorf("rebase = %+v, want carried from %s", retriedSteps[0], failedSteps[0].ID)
	}
	rounds, err := d.GetRoundsByStep(retriedSteps[1].ID)
	if err != nil || len(rounds) != 1 || rounds[0].Trigger != "retry" {
		t.Errorf("retried review rounds = %+v (err %v), want one retry round", rounds, err)
	}

	link, err := d.GetRunRetry(retried.ID)
	if err != nil || link == nil {
		t.Fatalf("audit link = %+v, err %v", link, err)
	}
	if link.SourceRunID != failed.ID || link.StepName != types.StepReview || link.SourceRound != 1 ||
		!strings.Contains(link.SourceError, "provider processing error") || link.CheckpointHeadSHA != preserved {
		t.Fatalf("audit link = %+v, want the failure, its step and round, and the checkpoint head", link)
	}
	if bySource, err := d.GetRunRetryBySource(failed.ID); err != nil || bySource == nil || bySource.RunID != retried.ID {
		t.Fatalf("source run's link = %+v (err %v), want it to name the continuation", bySource, err)
	}
	var info ipc.GetRunResult
	if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: retried.ID}, &info); err != nil || info.Run.RetryOf == nil || info.Run.RetryOf.RunID != failed.ID {
		t.Fatalf("retried run info = %+v (err %v), want retry_of %s", info.Run, err, failed.ID)
	}
	if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: failed.ID}, &info); err != nil || info.Run.RetriedBy != retried.ID {
		t.Fatalf("failed run info = %+v (err %v), want retried_by %s", info.Run, err, retried.ID)
	}
}

// retryFixture is a run that failed in Review on an agent invocation error,
// with every retry proof holding, so each refusal case breaks exactly one.
type retryFixture struct {
	p          *paths.Paths
	d          *db.DB
	repo       *db.Repo
	submitted  string
	source     *db.Run
	callerHead string
	executions *atomic.Int32
}

type retryFixtureOptions struct {
	beforeSource func(t *testing.T, f *retryFixture)
	profile      *agentcfg.PiProfile
	plan         *verificationplan.Snapshot
	failInTest   bool
	twoHop       bool
	globalYAML   string
	// repoYAML replaces the effective repository configuration the review
	// round records; empty records the configuration a new run would get.
	repoYAML   string
	invocation db.AgentInvocation
	// findings is the findings JSON the carried review round recorded.
	findings *string
}

func newRetryFixture(t *testing.T, opts retryFixtureOptions) *retryFixture {
	t.Helper()
	var executions atomic.Int32
	steps := []*countingStep{{name: types.StepRebase, count: &executions}, {name: types.StepReview, count: &executions}, {name: types.StepTest, count: &executions}}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{steps[0], steps[1], steps[2]}
	})
	repo, submitted := setupTestGitRepo(t, p, d, "retry-proof-repo")
	f := &retryFixture{p: p, d: d, repo: repo, submitted: submitted, callerHead: submitted, executions: &executions}
	if opts.beforeSource != nil {
		opts.beforeSource(t, f)
	}
	var profiles []*agentcfg.PiProfile
	if opts.profile != nil {
		profiles = append(profiles, opts.profile)
	}
	source, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "main", submitted, submitted, &db.RunIntent{Summary: "keep it", Source: db.RunIntentSourceAgent, Score: 1}, "", "", "", "", false, opts.plan, profiles...)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []types.StepName{types.StepRebase, types.StepReview, types.StepTest} {
		sr, err := d.InsertStepResult(source.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		const outage = "claude exited with status 1: provider processing error"
		switch {
		case name == types.StepRebase:
			err = d.CompleteStepWithStatus(sr.ID, types.StepStatusCompleted, 0, 1, "")
		case name == types.StepReview && opts.failInTest:
			if err = d.CompleteStepWithStatus(sr.ID, types.StepStatusCompleted, 0, 1, ""); err == nil {
				recorded := []byte(opts.repoYAML)
				if opts.repoYAML == "" {
					var effective *config.RepoConfig
					if effective, err = effectiveRepoConfigAt(context.Background(), p.RepoDir(repo.ID), repo, submitted, submitted, "test"); err == nil {
						recorded, err = yaml.Marshal(effective)
					}
				}
				if err == nil {
					_, err = d.InsertReviewStepRoundWithProvenance(sr.ID, 1, "initial", opts.findings, nil, submitted, "", submitted, []byte(opts.globalYAML), recorded, 1)
				}
			}
			if err == nil {
				inv := db.AgentInvocation{RunID: source.ID, StepName: string(types.StepReview), Round: 1, Purpose: "review", Agent: "claude", Model: "claude-opus-1", SessionMode: "fresh", StartedAt: 1, CompletedAt: 2, ExitStatus: "ok"}
				if opts.invocation.Agent != "" {
					inv.Agent, inv.Model, inv.Purpose, inv.Round = opts.invocation.Agent, opts.invocation.Model, opts.invocation.Purpose, opts.invocation.Round
				}
				_, err = d.InsertAgentInvocation(inv)
			}
		case name == types.StepReview:
			err = d.FailStepWithCause(sr.ID, "review agent: "+outage, 1, db.StepFailureCauseAgentInvocation)
		case name == types.StepTest && opts.failInTest:
			err = d.FailStepWithCause(sr.ID, "test agent: "+outage, 1, db.StepFailureCauseAgentInvocation)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := d.UpdateRunErrorStatusWithVerifiedHead(source.ID, "step review failed", types.RunFailed, submitted); err != nil {
		t.Fatal(err)
	}
	if opts.twoHop {
		source = f.continuationOf(t, source)
	}
	f.source, err = d.GetRun(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// continuationOf records a retry of source that carries its Review and failed
// in Test too, so its Review row holds no invocations of its own.
func (f *retryFixture) continuationOf(t *testing.T, source *db.Run) *db.Run {
	t.Helper()
	sourceRows, err := f.d.GetStepsByRun(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	next, err := f.d.InsertRunWithIntentAndLaunchNonce(f.repo.ID, "main", f.submitted, f.submitted, &db.RunIntent{Summary: "keep it", Source: db.RunIntentSourceAgent, Score: 1}, "", "", "", "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range sourceRows {
		if row.StepName == types.StepTest {
			sr, err := f.d.InsertStepResult(next.ID, row.StepName)
			if err == nil {
				err = f.d.FailStepWithCause(sr.ID, "test agent: usage limit", 1, db.StepFailureCauseAgentInvocation)
			}
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		if _, err := f.d.InsertCarriedStepResult(next.ID, row); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.d.UpdateRunErrorStatusWithVerifiedHead(next.ID, "step test failed", types.RunFailed, f.submitted); err != nil {
		t.Fatal(err)
	}
	return next
}

// writeFakeAgents points the named agents at executable stand-ins so a retry
// can resolve them, and writes extra global configuration after them.
func writeFakeAgents(t *testing.T, p *paths.Paths, selected string, extra string, names ...string) {
	t.Helper()
	config := "agent: " + selected + "\nagent_path_override:\n"
	for _, name := range names {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		config += "  " + name + ": " + path + "\n"
	}
	if err := os.WriteFile(p.ConfigFile(), []byte(config+extra), 0o644); err != nil {
		t.Fatal(err)
	}
}

type countingStep struct {
	name  types.StepName
	count *atomic.Int32
}

func (s *countingStep) Name() types.StepName { return s.name }

func (s *countingStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.count.Add(1)
	return &pipeline.StepOutcome{}, nil
}

func (f *retryFixture) retry(t *testing.T, runID string) (ipc.RetryRunResult, error) {
	t.Helper()
	client, err := ipc.Dial(f.p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.RetryRunResult
	err = client.Call(ipc.MethodRetryRun, &ipc.RetryRunParams{RepoID: f.repo.ID, Branch: "main", RunID: runID, CallerHeadSHA: f.callerHead}, &result)
	return result, err
}

func (f *retryFixture) stepRow(t *testing.T, name types.StepName) *db.StepResult {
	t.Helper()
	rows, err := f.d.GetStepsByRun(f.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.StepName == name {
			return row
		}
	}
	t.Fatalf("source run has no %s step", name)
	return nil
}

func TestRetryRefusesUnlessEveryProofHolds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		opts       retryFixtureOptions
		breakProof func(t *testing.T, f *retryFixture) string
		proof      string
		reason     string
	}{
		{name: "unknown run", proof: "run identity", breakProof: func(t *testing.T, f *retryFixture) string { return "missing-run" }},
		{name: "a run of another branch", proof: "run identity", breakProof: func(t *testing.T, f *retryFixture) string {
			other, err := f.d.InsertRun(f.repo.ID, "feature", f.submitted, f.submitted)
			if err == nil {
				err = f.d.UpdateRunErrorStatusWithVerifiedHead(other.ID, "step review failed", types.RunFailed, f.submitted)
			}
			if err != nil {
				t.Fatal(err)
			}
			return other.ID
		}},
		{name: "a newer run on the branch", proof: "latest run", breakProof: func(t *testing.T, f *retryFixture) string {
			newer, err := f.d.InsertRun(f.repo.ID, "main", f.submitted, f.submitted)
			if err == nil {
				err = f.d.UpdateRunStatus(newer.ID, types.RunFailed)
			}
			if err != nil {
				t.Fatal(err)
			}
			return f.source.ID
		}},
		{name: "an active run on the branch", proof: "no active run", opts: retryFixtureOptions{beforeSource: func(t *testing.T, f *retryFixture) {
			if _, err := f.d.InsertRun(f.repo.ID, "main", f.submitted, f.submitted); err != nil {
				t.Fatal(err)
			}
		}}},
		{name: "a cancelled run", proof: "failed run", breakProof: func(t *testing.T, f *retryFixture) string {
			if err := f.d.UpdateRunStatus(f.source.ID, types.RunCancelled); err != nil {
				t.Fatal(err)
			}
			return f.source.ID
		}},
		{name: "a run that pushed", proof: "unpublished run", breakProof: func(t *testing.T, f *retryFixture) string {
			if err := f.d.UpdateRunPushBinding(f.source.ID, db.PushBinding{HeadSHA: f.submitted, TargetKind: "origin", TargetFingerprint: "fp", Ref: "refs/heads/main"}); err != nil {
				t.Fatal(err)
			}
			return f.source.ID
		}},
		{name: "a failure about the change", proof: "eligible failure", breakProof: func(t *testing.T, f *retryFixture) string {
			if err := f.d.FailStep(f.stepRow(t, types.StepReview).ID, "review findings unreadable", 1); err != nil {
				t.Fatal(err)
			}
			return f.source.ID
		}},
		{name: "an incomplete record", proof: "durable checkpoint", breakProof: func(t *testing.T, f *retryFixture) string {
			if err := f.d.CompleteStepWithStatus(f.stepRow(t, types.StepTest).ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
				t.Fatal(err)
			}
			return f.source.ID
		}},
		{name: "an unverified final head", proof: "preserved head", breakProof: func(t *testing.T, f *retryFixture) string {
			if err := f.d.UpdateRunErrorStatus(f.source.ID, "step review failed", types.RunFailed); err != nil {
				t.Fatal(err)
			}
			return f.source.ID
		}},
		{name: "a missing recovery anchor", proof: "preserved head", reason: "is missing", breakProof: func(t *testing.T, f *retryFixture) string {
			gitCmd(t, f.repo.WorkingPath, "commit", "--allow-empty", "-m", "pipeline correction")
			moved := gitOutput(t, f.repo.WorkingPath, "rev-parse", "HEAD")
			gitCmd(t, f.repo.WorkingPath, "push", "gate", moved+":refs/heads/elsewhere")
			if err := f.d.UpdateRunErrorStatusWithVerifiedHead(f.source.ID, "step review failed", types.RunFailed, moved); err != nil {
				t.Fatal(err)
			}
			f.callerHead = moved
			return f.source.ID
		}},
		{name: "a moved recovery anchor", proof: "preserved head", reason: "points at", breakProof: func(t *testing.T, f *retryFixture) string {
			gitCmd(t, f.repo.WorkingPath, "commit", "--allow-empty", "-m", "pipeline correction")
			moved := gitOutput(t, f.repo.WorkingPath, "rev-parse", "HEAD")
			gitCmd(t, f.repo.WorkingPath, "push", "gate", moved+":refs/heads/elsewhere", f.submitted+":"+custody.RecoveryRef(f.source.ID))
			if err := f.d.UpdateRunErrorStatusWithVerifiedHead(f.source.ID, "step review failed", types.RunFailed, moved); err != nil {
				t.Fatal(err)
			}
			f.callerHead = moved
			return f.source.ID
		}},
		{name: "a dirty worktree", proof: "clean branch", breakProof: func(t *testing.T, f *retryFixture) string {
			f.callerHead = ""
			return f.source.ID
		}},
		{name: "a different local head", proof: "matching head", breakProof: func(t *testing.T, f *retryFixture) string {
			gitCmd(t, f.repo.WorkingPath, "commit", "--allow-empty", "-m", "local follow-up")
			f.callerHead = gitOutput(t, f.repo.WorkingPath, "rev-parse", "HEAD")
			return f.source.ID
		}},
		{name: "changed gates", proof: "compatible configuration", breakProof: func(t *testing.T, f *retryFixture) string {
			config := "auto_fix:\n  lint: 0\n  test: 0\n  review: 0\ngates:\n  - name: budget\n    after: test\n    command: \"true\"\n"
			if err := os.WriteFile(filepath.Join(f.repo.WorkingPath, ".no-mistakes.yaml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, f.repo.WorkingPath, "commit", "-am", "add a gate")
			gitCmd(t, f.repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
			gitCmd(t, f.repo.WorkingPath, "reset", "--hard", f.submitted)
			return f.source.ID
		}},
		{name: "a verification plan", proof: "compatible configuration",
			opts: retryFixtureOptions{plan: &verificationplan.Snapshot{ID: "plan-bound-run", Path: "plan.md", SourcePath: "plan.md", SHA256: "abc", CapturedAt: 1}}},
		{name: "a model profile that no longer resolves", proof: "compatible configuration",
			opts: retryFixtureOptions{profile: &agentcfg.PiProfile{Model: "openai-codex/gpt-5.4", Effort: agentcfg.EffortHigh}},
			breakProof: func(t *testing.T, f *retryFixture) string {
				if err := os.WriteFile(f.p.ConfigFile(), []byte("agent: pi\nagent_args_override:\n  pi: [\"--\"]\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return f.source.ID
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetryFixture(t, tc.opts)
			runID := f.source.ID
			if tc.breakProof != nil {
				runID = tc.breakProof(t, f)
			}
			before, err := f.d.GetRunsByRepo(f.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			gateRefs := gitOutput(t, f.p.RepoDir(f.repo.ID), "for-each-ref", "refs/heads", "refs/no-mistakes")
			callerHead := gitOutput(t, f.repo.WorkingPath, "rev-parse", "HEAD")

			result, err := f.retry(t, runID)
			if err == nil {
				t.Fatalf("retry started %s, want a refusal on the %s proof", result.RunID, tc.proof)
			}
			for _, want := range []string{"refusing retry of run " + runID, tc.proof + " proof failed", tc.reason, "no-mistakes rerun"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not contain %q", err, want)
				}
			}
			after, err := f.d.GetRunsByRepo(f.repo.ID)
			if err != nil || len(after) != len(before) {
				t.Fatalf("refusal changed the runs: before=%d after=%d err=%v", len(before), len(after), err)
			}
			if got := f.executions.Load(); got != 0 {
				t.Fatalf("refusal executed %d steps", got)
			}
			if got := gitOutput(t, f.p.RepoDir(f.repo.ID), "for-each-ref", "refs/heads", "refs/no-mistakes"); got != gateRefs {
				t.Fatalf("refusal changed the gate refs:\nbefore %s\nafter  %s", gateRefs, got)
			}
			if got := gitOutput(t, f.repo.WorkingPath, "rev-parse", "HEAD"); got != callerHead {
				t.Fatalf("refusal moved the caller head to %s", got)
			}
		})
	}
}

func TestRetryRefusesACarriedReviewFromAnotherConfiguration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the agent stand-ins are POSIX shell scripts")
	}
	for _, tc := range []struct {
		name   string
		opts   retryFixtureOptions
		change func(t *testing.T, f *retryFixture)
		reason []string
	}{
		{name: "trusted review settings changed", reason: []string{"repository configuration changed", "review"}, change: func(t *testing.T, f *retryFixture) {
			config := "review:\n  conversation: true\n"
			if err := os.WriteFile(filepath.Join(f.repo.WorkingPath, ".no-mistakes.yaml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, f.repo.WorkingPath, "add", ".no-mistakes.yaml")
			gitCmd(t, f.repo.WorkingPath, "commit", "-m", "tighten review")
			gitCmd(t, f.repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
			gitCmd(t, f.repo.WorkingPath, "reset", "--hard", f.submitted)
		}},
		{name: "selected agent changed", reason: []string{"no longer selected", "agent is now codex"}, change: func(t *testing.T, f *retryFixture) {
			writeFakeAgents(t, f.p, "codex", "", "claude", "codex")
		}},
		{name: "selected agent changed after a retry of a retry", opts: retryFixtureOptions{twoHop: true}, reason: []string{"no longer selected", "agent is now codex"}, change: func(t *testing.T, f *retryFixture) {
			writeFakeAgents(t, f.p, "codex", "", "claude", "codex")
		}},
		{name: "recorded repository configuration differs from the one a retry would use", opts: retryFixtureOptions{repoYAML: "review:\n  conversation: true\n"}, reason: []string{"repository configuration changed", "review"}, change: func(t *testing.T, f *retryFixture) {}},
		{name: "review role model changed", opts: retryFixtureOptions{invocation: db.AgentInvocation{Agent: "codex", Model: "gpt-old", Purpose: "review", Round: 1}}, reason: []string{"agent_config.codex.model is now gpt-new"}, change: func(t *testing.T, f *retryFixture) {
			writeFakeAgents(t, f.p, "claude", "review_agents:\n  reviewer:\n    agent: codex\n    model: gpt-new\n", "claude", "codex")
		}},
		{name: "agent model changed", reason: []string{"agent_config.claude.model is now sonnet"}, change: func(t *testing.T, f *retryFixture) {
			writeFakeAgents(t, f.p, "claude", "agent_config:\n  claude:\n    model: sonnet\n", "claude")
		}},
		{name: "recorded global configuration changed", opts: retryFixtureOptions{globalYAML: "agent: claude\n"}, reason: []string{"global configuration changed", "agent_path_override"}, change: func(t *testing.T, f *retryFixture) {
			writeFakeAgents(t, f.p, "claude", "", "claude")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.failInTest = true
			opts.beforeSource = func(t *testing.T, f *retryFixture) {
				if err := os.WriteFile(f.p.ConfigFile(), []byte("agent: claude\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			f := newRetryFixture(t, opts)
			tc.change(t, f)
			result, err := f.retry(t, f.source.ID)
			if err == nil {
				t.Fatalf("retry started %s, want a configuration refusal", result.RunID)
			}
			for _, want := range append([]string{"compatible configuration proof failed", "no-mistakes rerun"}, tc.reason...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not contain %q", err, want)
				}
			}
			if got := f.executions.Load(); got != 0 {
				t.Fatalf("refusal executed %d steps", got)
			}
		})
	}
}

func TestRetryContinuesAFailedTestWhenTheCarriedReviewConfigurationHolds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the agent stand-ins are POSIX shell scripts")
	}
	for _, twoHop := range []bool{false, true} {
		t.Run(fmt.Sprintf("two hop %v", twoHop), func(t *testing.T) {
			f := newRetryFixture(t, retryFixtureOptions{failInTest: true, twoHop: twoHop, beforeSource: func(t *testing.T, f *retryFixture) {
				writeFakeAgents(t, f.p, "claude", "agent_config:\n  claude:\n    model: opus\n", "claude")
			}})
			result, err := f.retry(t, f.source.ID)
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			if run := waitForRunTerminalState(t, f.d, result.RunID); run.Status != types.RunCompleted || result.Step != types.StepTest {
				t.Fatalf("retry = %+v, run = %s; want test continued to completion", result, run.Status)
			}
		})
	}
}

func TestRetryContinuesWhenTheCarriedReviewRanOnItsRoleAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the agent stand-ins are POSIX shell scripts")
	}
	for _, tc := range []struct {
		name       string
		invocation db.AgentInvocation
		extra      string
	}{
		{name: "reviewer role", invocation: db.AgentInvocation{Agent: "codex", Model: "gpt-5", Purpose: "review", Round: 1}, extra: "review_agents:\n  reviewer:\n    agent: codex\n    model: gpt-5\n"},
		{name: "reviewer takeover round", invocation: db.AgentInvocation{Agent: "codex", Model: "gpt-5", Purpose: "review", Round: 3}, extra: "review_agents:\n  reviewer_after_round:\n    agent: codex\n    model: gpt-5\n    after_round: 2\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetryFixture(t, retryFixtureOptions{failInTest: true, invocation: tc.invocation, beforeSource: func(t *testing.T, f *retryFixture) {
				writeFakeAgents(t, f.p, "claude", tc.extra, "claude", "codex")
			}})
			result, err := f.retry(t, f.source.ID)
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			if run := waitForRunTerminalState(t, f.d, result.RunID); run.Status != types.RunCompleted {
				t.Fatalf("run = %s, want completed", run.Status)
			}
		})
	}
}

func TestRetryStartsWhenEveryProofHolds(t *testing.T) {
	f := newRetryFixture(t, retryFixtureOptions{})
	result, err := f.retry(t, f.source.ID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	run := waitForRunTerminalState(t, f.d, result.RunID)
	if run.Status != types.RunCompleted || result.SourceRunID != f.source.ID || result.Step != types.StepReview || result.SourceRound != 1 {
		t.Fatalf("retry = %+v, run = %s; want review of %s continued to completion", result, run.Status, f.source.ID)
	}
	if got := f.executions.Load(); got != 2 {
		t.Fatalf("executed %d steps, want review and test only", got)
	}
}

func TestRunInfoReportsACarriedStepsWorkFromTheRunItWasCarriedFrom(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, _ := d.InsertRepo("/tmp/repo", "origin", "main")
	failed, _ := d.InsertRun(repo.ID, "main", "head", "base")
	review, err := d.InsertStepResult(failed.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"r1","severity":"error","description":"nil dereference"}],"summary":"1 error"}`
	applied := "changes applied"
	if _, err := d.InsertStepRound(review.ID, 1, "initial", &findings, nil, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(review.ID, 2, "auto_fix", nil, &applied, 1); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteStepWithStatus(review.ID, types.StepStatusCompleted, 0, 2, ""); err != nil {
		t.Fatal(err)
	}
	review, _ = d.GetStepResult(review.ID)
	continuation, _ := d.InsertRun(repo.ID, "main", "head", "base")
	carried, err := d.InsertCarriedStepResult(continuation.ID, review)
	if err != nil {
		t.Fatal(err)
	}
	info := stepToInfo(d, carried)
	if info.RoundCount != 2 || info.FixRoundCount != 1 || info.ReportedFindings != 1 || info.FixedFindings != 1 || len(info.FixSummaries) != 1 {
		t.Fatalf("carried review info = %+v, want the source run's two rounds, one fixed finding, and its fix summary", info)
	}
}

// conversationProbeStep records the review conversation its run can read.
type conversationProbeStep struct {
	mu        sync.Mutex
	open      []string
	withdrawn []string
}

func (s *conversationProbeStep) Name() types.StepName { return types.StepTest }

func (s *conversationProbeStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	conv, err := reviewqa.Load(reviewqa.Dir(sctx.EvidenceDir))
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range conv.Open() {
		s.open = append(s.open, entry.ID)
	}
	for _, entry := range conv.Withdrawn() {
		s.withdrawn = append(s.withdrawn, entry.ID)
	}
	return &pipeline.StepOutcome{}, nil
}

func (f *retryFixture) manager(t *testing.T, test pipeline.Step) *RunManager {
	t.Helper()
	m := NewRunManager(f.d, f.p, func() []pipeline.Step {
		return []pipeline.Step{&countingStep{name: types.StepRebase, count: f.executions}, &countingStep{name: types.StepReview, count: f.executions}, test}
	})
	t.Cleanup(m.Shutdown)
	return m
}

func (f *retryFixture) writeSourceConversation(t *testing.T) {
	t.Helper()
	dir := reviewqa.Dir(f.p.RunEvidenceDir("", f.source.ID))
	appendAgentQuestionLine(t, dir, `{"id":"q1","kind":"question","question":"keep the legacy route?","options":["keep","remove"],"weight":"major"}`)
	appendAgentQuestionLine(t, dir, `{"id":"q2","kind":"question","question":"rename the table?","options":["yes","no"],"weight":"major"}`)
	appendAgentQuestionLine(t, dir, `{"id":"q2","kind":"retract","reason":"answered by the migration note"}`)
}

const reviewQuestionFindings = `{"findings":[{"id":"question-q1","severity":"warning","description":"keep the legacy route?","action":"ask-user","category":"review-question"}],"summary":"one question"}`

func TestRetryCarriesTheReviewConversationOfACarriedReview(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the agent stand-ins are POSIX shell scripts")
	}
	findings := reviewQuestionFindings
	f := newRetryFixture(t, retryFixtureOptions{failInTest: true, findings: &findings, beforeSource: func(t *testing.T, f *retryFixture) {
		writeFakeAgents(t, f.p, "claude", "", "claude")
	}})
	f.source, _ = f.d.GetRun(f.source.ID)
	f.writeSourceConversation(t)
	probe := &conversationProbeStep{}
	link, err := f.manager(t, probe).HandleRetry(context.Background(), f.repo.ID, "main", f.source.ID, f.callerHead)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if run := waitForRunTerminalState(t, f.d, link.RunID); run.Status != types.RunCompleted {
		t.Fatalf("continuation = %s, want completed", run.Status)
	}
	if !slices.Equal(probe.open, []string{"q1"}) || !slices.Equal(probe.withdrawn, []string{"q2"}) {
		t.Fatalf("continuation read open %v, withdrawn %v; want the open q1 and the withdrawn q2", probe.open, probe.withdrawn)
	}
}

func TestRetryRefusesACarriedReviewWhoseQuestionsCannotBeRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the agent stand-ins are POSIX shell scripts")
	}
	findings := reviewQuestionFindings
	f := newRetryFixture(t, retryFixtureOptions{failInTest: true, findings: &findings, beforeSource: func(t *testing.T, f *retryFixture) {
		writeFakeAgents(t, f.p, "claude", "", "claude")
	}})
	_, err := f.manager(t, &conversationProbeStep{}).HandleRetry(context.Background(), f.repo.ID, "main", f.source.ID, f.callerHead)
	if err == nil {
		t.Fatal("retry started, want a refusal: the carried review asked a question and its conversation is gone")
	}
	for _, want := range []string{"review conversation proof failed", "no-mistakes rerun"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err, want)
		}
	}
	f.requireNothingLaunched(t)
}

func TestRetryRefusedAtLaunchCanBeRetriedOnceTheCauseIsFixed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the agent stand-ins are POSIX shell scripts")
	}
	findings := reviewQuestionFindings
	f := newRetryFixture(t, retryFixtureOptions{failInTest: true, findings: &findings, beforeSource: func(t *testing.T, f *retryFixture) {
		writeFakeAgents(t, f.p, "claude", "", "claude")
	}})
	m := f.manager(t, &conversationProbeStep{})
	if _, err := m.HandleRetry(context.Background(), f.repo.ID, "main", f.source.ID, f.callerHead); err == nil {
		t.Fatal("retry started, want a refusal: the conversation is gone")
	}
	f.requireNothingLaunched(t)

	f.writeSourceConversation(t)
	link, err := m.HandleRetry(context.Background(), f.repo.ID, "main", f.source.ID, f.callerHead)
	if err != nil {
		t.Fatalf("second retry after restoring the conversation: %v", err)
	}
	if run := waitForRunTerminalState(t, f.d, link.RunID); run.Status != types.RunCompleted {
		t.Fatalf("continuation = %s, want completed", run.Status)
	}
}

func TestRetryRefusesWhenTheConfigurationChangesBetweenTheProofAndTheLaunch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the agent stand-ins are POSIX shell scripts")
	}
	f := newRetryFixture(t, retryFixtureOptions{failInTest: true, beforeSource: func(t *testing.T, f *retryFixture) {
		writeFakeAgents(t, f.p, "claude", "", "claude")
	}})
	m := f.manager(t, &conversationProbeStep{})
	m.afterRetryProof = func() {
		if err := os.WriteFile(filepath.Join(f.repo.WorkingPath, ".no-mistakes.yaml"), []byte("review:\n  conversation: true\n"), 0o644); err != nil {
			t.Error(err)
		}
		gitCmd(t, f.repo.WorkingPath, "add", ".no-mistakes.yaml")
		gitCmd(t, f.repo.WorkingPath, "commit", "-m", "tighten review")
		gitCmd(t, f.repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
		gitCmd(t, f.repo.WorkingPath, "reset", "--hard", f.submitted)
	}
	_, err := m.HandleRetry(context.Background(), f.repo.ID, "main", f.source.ID, f.callerHead)
	if err == nil {
		t.Fatal("retry started, want a refusal: the trusted settings changed after the proof")
	}
	for _, want := range []string{"compatible configuration proof failed", "repository configuration changed", "review", "no-mistakes rerun"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err, want)
		}
	}
	f.requireNothingLaunched(t)
}

// requireNothingLaunched fails when a refused retry left an audit link, a
// carried step, or an executed step behind.
func (f *retryFixture) requireNothingLaunched(t *testing.T) {
	t.Helper()
	if link, err := f.d.GetRunRetryBySource(f.source.ID); err != nil || link != nil {
		t.Errorf("run_retries row = %+v (err %v), want none", link, err)
	}
	runs, err := f.d.GetRunsByRepo(f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.ID == f.source.ID {
			continue
		}
		t.Errorf("refused retry left run %s (%s) behind, want only the failed run", run.ID, run.Status)
		rows, err := f.d.GetStepsByRun(run.ID)
		if err != nil || len(rows) != 0 {
			t.Errorf("refused continuation %s has steps %+v (err %v), want none", run.ID, rows, err)
		}
	}
	if got := f.executions.Load(); got != 0 {
		t.Errorf("refusal executed %d steps", got)
	}
}
