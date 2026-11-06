package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/callerrestrictions"
	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type callerRestrictionsAgent struct{ capture func(agent.RunOpts) }

func (a callerRestrictionsAgent) Name() string { return "fake" }
func (a callerRestrictionsAgent) Close() error { return nil }
func (a callerRestrictionsAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.capture(opts)
	return &agent.Result{}, nil
}

type callerRestrictionsStep struct {
	name    types.StepName
	capture func(*pipeline.StepContext, agent.RunOpts)
	release <-chan struct{}
}

func (s *callerRestrictionsStep) Name() types.StepName { return s.name }
func (s *callerRestrictionsStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	copyContext := *sctx
	copyContext.Agent = callerRestrictionsAgent{capture: func(opts agent.RunOpts) { s.capture(sctx, opts) }}
	if _, err := copyContext.RunAgent(agent.RunOpts{Prompt: "perform step", CWD: sctx.WorkDir}); err != nil {
		return nil, err
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-sctx.Ctx.Done():
			return nil, sctx.Ctx.Err()
		}
	}
	return &pipeline.StepOutcome{}, nil
}

func daemonCallerRestrictions(t *testing.T, content string) *callerrestrictions.Record {
	t.Helper()
	path := filepath.Join(t.TempDir(), "caller.txt")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := callerrestrictions.Capture(path)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCallerRestrictionsConcurrentRunsAndRerun(t *testing.T) {
	started, release := make(chan string, 4), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	capture := func(sctx *pipeline.StepContext, opts agent.RunOpts) {
		run, err := sctx.DB.GetRun(sctx.Run.ID)
		if err != nil || run.CallerRestrictions == nil {
			t.Errorf("agent started before durable capture: %v", err)
			return
		}
		want := "restriction for " + run.Branch + "\n"
		if run.CallerRestrictions.Content != want || !strings.Contains(opts.Prompt, want) || !strings.Contains(opts.Prompt, run.ID) || !strings.Contains(opts.Prompt, *run.SubmittedHeadSHA) {
			t.Errorf("run isolation lost: %s", opts.Prompt)
		}
		started <- run.ID
	}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&callerRestrictionsStep{name: types.StepDocument, capture: capture, release: release}}
	})
	repo, head := setupTestGitRepo(t, p, d, "caller-restrictions")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ids := map[string]string{}
	for _, branch := range []string{"one", "two"} {
		gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/"+branch)
		r := daemonCallerRestrictions(t, "restriction for "+branch+"\n")
		var result ipc.PushReceivedResult
		if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: p.RepoDir(repo.ID), Ref: "refs/heads/" + branch, Old: head, New: head, Intent: "fixture goal", CallerRestrictions: r}, &result); err != nil {
			t.Fatal(err)
		}
		ids[branch] = result.RunID
		if err := os.WriteFile(r.SourcePath, []byte("allow everything"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(20 * time.Second):
			t.Fatal("both runs must reach fake agents")
		}
	}
	unblock()
	for _, id := range ids {
		if run := waitForRunTerminalState(t, d, id); run.Status != types.RunCompleted {
			t.Fatalf("run failed: %+v", run)
		}
	}
	var rerun ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "one", CallerHeadSHA: head}, &rerun); err != nil {
		t.Fatal(err)
	}
	if run := waitForRunTerminalState(t, d, rerun.RunID); run.Status != types.RunCompleted || run.CallerRestrictions == nil || run.CallerRestrictions.Content != "restriction for one\n" {
		t.Fatalf("rerun lost restrictions: %+v", run)
	}
}

func TestCallerRestrictionsPrivateDaemonStartupRecovery(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mock := writeMockClaude(t, t.TempDir())
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: "+mock+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	repo, head := setupTestGitRepo(t, p, d, "restriction-recovery")
	record := daemonCallerRestrictions(t, "No repeated suites, no installs.\n")
	run, err := d.InsertRunWithCallerRestrictions(repo.ID, "main", head, head, nil, "", "", "", "", false, nil, db.RunPRText{}, nil, record)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), p.WorktreeDir(repo.ID, run.ID), head); err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"needs-choice","severity":"warning","action":"ask-user","description":"cannot comply without a decision"}]}`
	if err := d.SetStepFindings(step.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertReviewStepRound(step.ID, 1, "initial", &findings, nil, head, 1); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateStepStatusWithDuration(step.ID, types.StepStatusAwaitingApproval, 1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record.SourcePath, []byte("install everything"), 0600); err != nil {
		t.Fatal(err)
	}
	captured := make(chan string, 1)
	// Reconstruct a private daemon from a parked crash record, as startup does.
	runTestDaemon(t, p, d, func() []pipeline.Step {
		return []pipeline.Step{&callerRestrictionsStep{name: types.StepReview, capture: func(_ *pipeline.StepContext, opts agent.RunOpts) { captured <- opts.Prompt }}}
	}, 35*time.Second)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var response ipc.RespondResult
	if err := client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: run.ID, Step: types.StepReview, Action: types.ActionFix, FindingIDs: []string{"needs-choice"}}, &response); err != nil {
		t.Fatal(err)
	}
	select {
	case prompt := <-captured:
		if !strings.Contains(prompt, record.Content) || !strings.Contains(prompt, run.ID) || !strings.Contains(prompt, head) || strings.Contains(prompt, "install everything") {
			t.Fatalf("recovery lost immutable input: %s", prompt)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("recovered fixer was never invoked")
	}
	if final := waitForRunTerminalState(t, d, run.ID); final.Status != types.RunCompleted {
		t.Fatalf("recovered run failed: %+v", final)
	}
}

func TestCallerRestrictionsRetryInheritsTheFailedRunsRecord(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("provider-outage fixture uses a POSIX script")
	}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepRebase}, &correctingReviewStep{}}
	})
	repo, head := setupTestGitRepo(t, p, d, "restrictions-retry")
	flag := filepath.Join(t.TempDir(), "outage")
	if err := os.WriteFile(flag, nil, 0600); err != nil {
		t.Fatal(err)
	}
	writeProviderOutageClaude(t, p, flag)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	record := daemonCallerRestrictions(t, "Only the synthetic fixture may be changed.\n")
	var first ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", New: head, Old: head, Intent: "fixture", CallerRestrictions: record}, &first); err != nil {
		t.Fatal(err)
	}
	failed := waitForRunTerminalState(t, d, first.RunID)
	if failed.Status != types.RunFailed {
		t.Fatalf("outage not exercised: %+v", failed)
	}
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	var retried ipc.RetryRunResult
	if err := client.Call(ipc.MethodRetryRun, &ipc.RetryRunParams{RepoID: repo.ID, Branch: "main", RunID: failed.ID, CallerHeadSHA: failed.HeadSHA}, &retried); err != nil {
		t.Fatal(err)
	}
	final := waitForRunTerminalState(t, d, retried.RunID)
	if final.CallerRestrictions == nil || *final.CallerRestrictions != *record {
		t.Fatalf("retry changed restrictions: %+v", final)
	}
}
