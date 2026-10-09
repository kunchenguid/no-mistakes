package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type boundDaemonStep struct {
	name types.StepName
	run  func(*pipeline.StepContext) (*pipeline.StepOutcome, error)
}

func (s *boundDaemonStep) Name() types.StepName { return s.name }

func (s *boundDaemonStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return s.run(sctx)
}

func TestBoundRevalidationThroughRegisteredDaemonHandlers(t *testing.T) {
	root, err := os.MkdirTemp("", "nm-bound-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	workDir := t.TempDir()
	gitCmd(t, workDir, "init")
	gitCmd(t, workDir, "config", "user.name", "test")
	gitCmd(t, workDir, "config", "user.email", "test@test.com")
	gitCmd(t, workDir, "checkout", "-b", "feature/bound")
	if err := os.WriteFile(filepath.Join(workDir, "submitted.txt"), []byte("submitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", "submitted.txt")
	gitCmd(t, workDir, "commit", "-m", "submitted")
	headSHA := gitOutput(t, workDir, "rev-parse", "HEAD")
	gitCmd(t, workDir, "checkout", "--detach", headSHA)

	repo, err := database.InsertRepoWithID("repo-bound", workDir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/bound", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}

	var order []types.StepName
	pass := func(name types.StepName) pipeline.Step {
		return &boundDaemonStep{name: name, run: func(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
			order = append(order, name)
			return &pipeline.StepOutcome{}, nil
		}}
	}
	push := &boundDaemonStep{name: types.StepPush, run: func(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
		order = append(order, types.StepPush)
		if err := database.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: run.HeadSHA, TargetKind: "upstream", TargetFingerprint: "target", Ref: "refs/heads/feature/bound"}); err != nil {
			return nil, err
		}
		return &pipeline.StepOutcome{}, nil
	}}
	pr := &boundDaemonStep{name: types.StepPR, run: func(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
		order = append(order, types.StepPR)
		if err := database.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/1"); err != nil {
			return nil, err
		}
		return &pipeline.StepOutcome{}, nil
	}}
	var ciCalls atomic.Int32
	repairContext := make(chan *pipeline.StepContext, 1)
	ci := &boundDaemonStep{name: types.StepCI, run: func(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		order = append(order, types.StepCI)
		switch ciCalls.Add(1) {
		case 1:
			return &pipeline.StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"ci-1","severity":"error","description":"repair CI","action":"ask-user","category":"ci-check","check":"test"}]}`}, nil
		case 2:
			repairContext <- sctx
			return &pipeline.StepOutcome{RestartFrom: types.StepReview}, nil
		default:
			return &pipeline.StepOutcome{}, nil
		}
	}}
	steps := []pipeline.Step{pass(types.StepReview), pass(types.StepTest), pass(types.StepDocument), pass(types.StepLint), push, pr, ci}
	exec := pipeline.NewExecutor(database, p, &config.Config{}, nil, steps, nil)
	mgr := NewRunManager(database, p, nil)
	mgr.mu.Lock()
	mgr.executors[run.ID] = exec
	mgr.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var executorDone atomic.Bool
	go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
	t.Cleanup(func() {
		cancel()
		if executorDone.Load() {
			return
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("bound-response executor did not stop")
		}
	})

	var ciResult *db.StepResult
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		results, readErr := database.GetStepsByRun(run.ID)
		if readErr == nil {
			for _, result := range results {
				if result.StepName == types.StepCI && result.Status == types.StepStatusAwaitingApproval {
					ciResult = result
					break
				}
			}
		}
		if ciResult != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ciResult == nil {
		t.Fatal("CI gate did not park")
	}
	rounds, err := database.GetRoundsByStep(ciResult.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("CI rounds = %#v err=%v", rounds, err)
	}

	srv := ipc.NewServer()
	registerHandlers(srv, mgr, database, func() {})
	if err := srv.Listen(p.Socket()); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.ServeReady() }()
	t.Cleanup(func() {
		srv.Close()
		<-served
	})
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var probe ipc.ProbeBoundRevalidationResult
	if err := client.Call(ipc.MethodProbeBoundRevalidation, nil, &probe); err != nil || !probe.OK {
		t.Fatalf("capability probe = %#v err=%v", probe, err)
	}

	params := ipc.RespondParams{
		RunID: run.ID, Step: types.StepCI, Action: types.ActionFix, FindingIDs: []string{"ci-1"},
		RequireReviewRevalidation: true, OperationID: "operation-daemon", ExpectedRepoID: repo.ID,
		ExpectedBranch: run.Branch, ExpectedHeadSHA: run.HeadSHA, ExpectedStepResultID: ciResult.ID, ExpectedRoundID: rounds[0].ID,
	}
	var first ipc.RespondResult
	if err := client.Call(ipc.MethodRespond, &params, &first); err != nil {
		t.Fatalf("bound response: %v", err)
	}
	if !first.OK || first.Replayed || first.Operation == nil || first.Operation.ID != params.OperationID || !slices.Equal(first.Fixed, []string{"ci-1"}) {
		t.Fatalf("bound response = %#v", first)
	}
	select {
	case got := <-repairContext:
		if !got.Fixing || !got.RequireReviewRevalidation {
			t.Fatalf("repair context = fixing:%v require_review:%v", got.Fixing, got.RequireReviewRevalidation)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bound repair did not dispatch")
	}
	select {
	case err := <-done:
		executorDone.Store(true)
		if err != nil {
			t.Fatalf("pipeline: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pipeline did not complete revalidation")
	}

	expectedOrder := []types.StepName{
		types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI,
		types.StepCI, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI,
	}
	if !slices.Equal(order, expectedOrder) {
		t.Fatalf("pipeline order = %v, want %v", order, expectedOrder)
	}
	fingerprint, err := boundRespondFingerprint(params)
	if err != nil {
		t.Fatal(err)
	}
	durable, err := database.GetBoundResponseOperation(params.OperationID)
	if err != nil || durable == nil || durable.Fingerprint != fingerprint {
		t.Fatalf("durable receipt = %#v err=%v, want fingerprint %s", durable, err, fingerprint)
	}
	binding := pipeline.ResponseRevalidationBinding{
		OperationID: params.OperationID, Fingerprint: fingerprint, RunID: params.RunID,
		RepoID: params.ExpectedRepoID, Branch: params.ExpectedBranch, HeadSHA: params.ExpectedHeadSHA,
		StepResultID: params.ExpectedStepResultID, RoundID: params.ExpectedRoundID,
	}
	if !pipeline.BoundResponseMatches(durable, &binding) {
		t.Fatalf("durable receipt no longer matches retry: operation=%#v binding=%#v", durable, binding)
	}

	mgr.mu.Lock()
	delete(mgr.executors, run.ID)
	mgr.mu.Unlock()
	var replay ipc.RespondResult
	if err := client.Call(ipc.MethodRespond, &params, &replay); err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	if !replay.OK || !replay.Replayed || replay.Operation == nil || replay.Operation.AcceptedAt != first.Operation.AcceptedAt || !slices.Equal(replay.Fixed, first.Fixed) {
		t.Fatalf("replay = %#v, first = %#v", replay, first)
	}
	changed := params
	changed.Instructions = map[string]string{"ci-1": "different payload"}
	if err := client.Call(ipc.MethodRespond, &changed, &ipc.RespondResult{}); err == nil {
		t.Fatal("changed retry was accepted")
	}
	if got := ciCalls.Load(); got != 3 {
		t.Fatalf("CI dispatch count = %d, want initial, one repair, and final observation", got)
	}
}
