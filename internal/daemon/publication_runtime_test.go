package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	pipelinesteps "github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/publication"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type runtimeCandidateGuard struct{}

func (runtimeCandidateGuard) Inspect(context.Context, string, types.StepName) (publication.CandidateSnapshot, error) {
	return publication.CandidateSnapshot{
		CommitSHA:         strings.Repeat("c", 40),
		TreeSHA:           strings.Repeat("d", 40),
		TrackedClean:      true,
		IndexClean:        true,
		UntrackedClean:    true,
		RefsSHA256:        strings.Repeat("1", 64),
		ConfigSHA256:      strings.Repeat("2", 64),
		ReplaceRefsSHA256: strings.Repeat("3", 64),
	}, nil
}

type runtimePushPort struct {
	mu              sync.Mutex
	publishes       int
	remoteHead      string
	observeFailures int
}

func (p *runtimePushPort) PublishExact(_ context.Context, request publication.PushEffectRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.publishes++
	p.remoteHead = request.CommitSHA
	return nil
}

func (p *runtimePushPort) ObserveExact(context.Context, publication.PushEffectRequest) (publication.PushObservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.observeFailures > 0 {
		p.observeFailures--
		return publication.PushObservation{}, errors.New("observation interrupted")
	}
	return publication.PushObservation{RemoteHeadSHA: p.remoteHead}, nil
}

func (p *runtimePushPort) publishCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.publishes
}

type runtimePRPort struct {
	mu              sync.Mutex
	creates         int
	created         *publication.PREffectRequest
	observeFailures int
}

func (p *runtimePRPort) CreateExact(_ context.Context, request publication.PREffectRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates++
	copy := request
	p.created = &copy
	return nil
}

func (p *runtimePRPort) FindExact(_ context.Context, query publication.PRReconcileQuery) ([]publication.PRObservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.observeFailures > 0 {
		p.observeFailures--
		return nil, errors.New("PR observation interrupted")
	}
	if p.created == nil {
		return nil, nil
	}
	return []publication.PRObservation{{
		RepositoryID: query.RepositoryID,
		BaseRef:      query.BaseRef,
		HeadRef:      query.HeadRef,
		HeadSHA:      query.CommitSHA,
		Marker:       query.Marker,
		DraftSHA256:  query.DraftSHA256,
		Number:       "1",
	}}, nil
}

func (p *runtimePRPort) createCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.creates
}

type runtimeCIPort struct{}

func (runtimeCIPort) ObserveExact(_ context.Context, query publication.CIQuery) (publication.CIObservation, error) {
	return publication.CIObservation{
		HeadSHA: query.CommitSHA,
		Checks:  []publication.CICheck{{Name: "test", HeadSHA: query.CommitSHA, Status: publication.CICheckPass}},
	}, nil
}

type runtimeStep struct{ name types.StepName }

func (s runtimeStep) Name() types.StepName { return s.name }
func (s runtimeStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return nil, errors.New("ordinary step execution escaped the publication adapter")
}

type runtimePassingStep struct{ name types.StepName }

func (s runtimePassingStep) Name() types.StepName { return s.name }
func (s runtimePassingStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	outcome := &pipeline.StepOutcome{}
	if s.name == types.StepReview {
		outcome.ReviewApprovedHeadSHA = sctx.Run.HeadSHA
	}
	return outcome, nil
}

func passingRuntimeSteps() []pipeline.Step {
	result := make([]pipeline.Step, 0, len(types.AllSteps()))
	for _, name := range types.AllSteps() {
		result = append(result, runtimePassingStep{name: name})
	}
	return result
}

type runtimeCandidatePort struct {
	root  string
	guard runtimeCandidateGuard
	mu    sync.Mutex
	views map[types.StepName]publication.CandidateStepView
}

func (p *runtimeCandidatePort) PrepareStep(_ context.Context, _ string, step types.StepName) (publication.CandidateStepView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	view := publication.CandidateStepView{
		WorktreeDir:     filepath.Join(p.root, "candidates", string(step)),
		ScratchDir:      filepath.Join(p.root, "scratch", string(step)),
		WorkContractRaw: []byte("version = 1\n"),
	}
	if err := os.MkdirAll(filepath.Dir(view.WorktreeDir), 0o700); err != nil {
		return publication.CandidateStepView{}, err
	}
	if err := os.Mkdir(view.WorktreeDir, 0o500); err != nil {
		return publication.CandidateStepView{}, err
	}
	if err := os.MkdirAll(filepath.Dir(view.ScratchDir), 0o700); err != nil {
		return publication.CandidateStepView{}, err
	}
	if err := os.Mkdir(view.ScratchDir, 0o700); err != nil {
		return publication.CandidateStepView{}, err
	}
	if p.views == nil {
		p.views = make(map[types.StepName]publication.CandidateStepView)
	}
	p.views[step] = view
	return view, nil
}

func (p *runtimeCandidatePort) Inspect(ctx context.Context, publicationID string, step types.StepName) (publication.CandidateSnapshot, error) {
	p.mu.Lock()
	_, exists := p.views[step]
	p.mu.Unlock()
	if !exists {
		return publication.CandidateSnapshot{}, errors.New("candidate view is not prepared")
	}
	return p.guard.Inspect(ctx, publicationID, step)
}

func (p *runtimeCandidatePort) DisposeStep(_ context.Context, _ string, step types.StepName) error {
	p.mu.Lock()
	delete(p.views, step)
	p.mu.Unlock()
	return nil
}

type runtimeFreshnessPort struct{}

func (runtimeFreshnessPort) CheckUpToDate(context.Context, string, publication.CandidateStepView) error {
	return nil
}

func runtimeSteps() []pipeline.Step {
	steps := make([]pipeline.Step, 0, len(types.AllSteps()))
	for _, name := range types.AllSteps() {
		steps = append(steps, runtimeStep{name: name})
	}
	return steps
}

type recordingPublicationAdapter struct {
	mu      sync.Mutex
	calls   []types.StepName
	entered chan struct{}
	release <-chan struct{}
}

func (a *recordingPublicationAdapter) ExecutePublicationStep(sctx *pipeline.StepContext, step pipeline.Step) (*pipeline.StepOutcome, error) {
	a.mu.Lock()
	first := len(a.calls) == 0
	a.calls = append(a.calls, step.Name())
	a.mu.Unlock()
	if first && a.entered != nil {
		close(a.entered)
	}
	if first && a.release != nil {
		select {
		case <-sctx.Ctx.Done():
			return nil, sctx.Ctx.Err()
		case <-a.release:
		}
	}
	outcome := &pipeline.StepOutcome{}
	if step.Name() == types.StepReview {
		outcome.ReviewApprovedHeadSHA = sctx.Run.HeadSHA
	}
	return outcome, nil
}

func (a *recordingPublicationAdapter) snapshot() []types.StepName {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]types.StepName(nil), a.calls...)
}

type publicationRuntimeFixture struct {
	database *db.DB
	paths    *paths.Paths
	manager  *publication.Manager
	runs     *RunManager
	parsed   publication.ParsedRequest
	push     *runtimePushPort
	pr       *runtimePRPort
}

func newPublicationRuntimeFixture(t *testing.T, suffix string) publicationRuntimeFixture {
	t.Helper()
	root := t.TempDir()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure runtime paths: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open runtime database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	repoID := "abcdeffedcba"
	if _, err := database.InsertRepoWithID(repoID, filepath.Join(root, "source"), "https://github.com/example/project.git", "main"); err != nil {
		t.Fatalf("insert runtime repo: %v", err)
	}
	request := publication.Request{
		Protocol: publication.ProtocolV1,
		Factory: publication.FactoryBinding{
			RunID:                "factory-" + suffix,
			TerminalT10Sequence:  10,
			RunStatePrefixSHA256: strings.Repeat("1", 64),
			PlanBindingSHA256:    strings.Repeat("2", 64),
		},
		WorkContract: publication.WorkContractBinding{Path: "WORK-CONTRACT.json", SHA256: strings.Repeat("3", 64)},
		BuildIntent:  publication.BuildIntentProjection{Summary: "publish exact candidate", AcceptanceCriteria: []string{"exact H"}},
		Candidate: publication.CandidateBinding{
			RepositoryID: repoID,
			HeadRef:      "refs/heads/feature/" + suffix,
			BaseRef:      "refs/heads/main",
			BaseSHA:      strings.Repeat("b", 40),
			CommitSHA:    strings.Repeat("c", 40),
			TreeSHA:      strings.Repeat("d", 40),
		},
		Publisher: publication.PublisherBinding{
			ExecutablePath:   "/opt/pinned/no-mistakes",
			ExecutableSHA256: strings.Repeat("e", 64),
			BuildSHA:         strings.Repeat("f", 40),
			Protocol:         publication.ProtocolV1,
		},
		Scopes: publication.PublicationScopes{
			Push: publication.PushScope{Mode: publication.PushModeExactCommit, RemoteIdentity: "github.com/example/project", DestinationRef: "refs/heads/feature/" + suffix},
			PR:   publication.PRScope{Mode: publication.PRModeCreateOrUpdateExactHead, BaseRef: "refs/heads/main", HeadRef: "refs/heads/feature/" + suffix},
			CI:   publication.CIScope{Mode: publication.CIModeObserveExactHead},
		},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := publication.ParseRequest(raw)
	if err != nil {
		t.Fatalf("parse runtime request: %v", err)
	}
	push := &runtimePushPort{}
	pr := &runtimePRPort{}
	manager, err := publication.NewManager(publication.ManagerDeps{
		DB: database, Candidate: runtimeCandidateGuard{}, Push: push, PR: pr, CI: runtimeCIPort{},
	})
	if err != nil {
		t.Fatalf("new publication manager: %v", err)
	}
	return publicationRuntimeFixture{
		database: database,
		paths:    p,
		manager:  manager,
		runs:     NewRunManager(database, p, nil),
		parsed:   parsed,
		push:     push,
		pr:       pr,
	}
}

func publicationExecutorFactoryForTest(t *testing.T, fixture publicationRuntimeFixture, adapter pipeline.PublicationStepAdapter, calls *atomic.Int32) publicationExecutorFactory {
	t.Helper()
	return func(_ context.Context, _ string, _ *db.Run, _ *db.Repo) (*publicationExecutorPlan, error) {
		calls.Add(1)
		executor := pipeline.NewExecutor(fixture.database, fixture.paths, &config.Config{}, nil, runtimeSteps(), nil)
		executor.SetPublicationStepAdapter(adapter)
		return &publicationExecutorPlan{Executor: executor}, nil
	}
}

func TestPublicationRuntimeConcurrentStartLaunchesExactlyOneExecutor(t *testing.T) {
	fixture := newPublicationRuntimeFixture(t, "one-executor")
	release := make(chan struct{})
	adapter := &recordingPublicationAdapter{entered: make(chan struct{}), release: release}
	var factories atomic.Int32
	runtime, err := newPublicationRuntime(publicationRuntimeOptions{
		DB: fixture.database, Runs: fixture.runs, Manager: fixture.manager,
		Identity:        fixture.parsed.Request.Publisher,
		ExecutorFactory: publicationExecutorFactoryForTest(t, fixture, adapter, &factories),
	})
	if err != nil {
		t.Fatalf("new publication runtime: %v", err)
	}

	const callers = 16
	results := make(chan publication.Result, callers)
	errors := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := runtime.Start(context.Background(), fixture.parsed)
			if err != nil {
				errors <- err
				return
			}
			results <- result
		}()
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Errorf("concurrent start: %v", err)
	}
	for result := range results {
		if result.PublicationID != fixture.parsed.PublicationID {
			t.Errorf("publication ID = %q", result.PublicationID)
		}
	}
	select {
	case <-adapter.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publication executor did not start")
	}
	if got := factories.Load(); got != 1 {
		t.Fatalf("executor factory calls = %d, want exactly one", got)
	}
	close(release)
	waitPublicationRuntimeIdle(t, fixture.runs)
}

func TestPublicationRuntimeFactoryFailureTerminalizesTheDurableRun(t *testing.T) {
	cases := map[string]publicationExecutorFactory{
		"factory error": func(context.Context, string, *db.Run, *db.Repo) (*publicationExecutorPlan, error) {
			return nil, errors.New("composition refused")
		},
		"missing executor": func(context.Context, string, *db.Run, *db.Repo) (*publicationExecutorPlan, error) {
			return &publicationExecutorPlan{}, nil
		},
	}
	for name, factory := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newPublicationRuntimeFixture(t, "factory-failure-"+strings.ReplaceAll(name, " ", "-"))
			runtime, err := newPublicationRuntime(publicationRuntimeOptions{
				DB: fixture.database, Runs: fixture.runs, Manager: fixture.manager,
				Identity: fixture.parsed.Request.Publisher, ExecutorFactory: factory,
			})
			if err != nil {
				t.Fatal(err)
			}

			if _, err := runtime.Start(context.Background(), fixture.parsed); err == nil {
				t.Fatal("publication start unexpectedly survived executor composition failure")
			}
			publicationRow, err := fixture.database.GetPublication(fixture.parsed.PublicationID)
			if err != nil || publicationRow == nil {
				t.Fatalf("load admitted publication: row=%#v err=%v", publicationRow, err)
			}
			run, err := fixture.database.GetRun(publicationRow.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if run == nil || run.Status != types.RunFailed || run.Error == nil || *run.Error == "" {
				t.Fatalf("composition failure left publication nonterminal: %#v", run)
			}
			if fixture.runs.publicationExecutorActive(publicationRow.RunID) {
				t.Fatal("composition failure registered a publication executor")
			}
		})
	}
}

func TestPublicationRuntimeStatusIsPureProjectionAndAuthorizeDoesNotLaunch(t *testing.T) {
	fixture := newPublicationRuntimeFixture(t, "projection")
	if _, err := fixture.manager.Start(context.Background(), fixture.parsed); err != nil {
		t.Fatalf("admit directly through manager: %v", err)
	}
	var factories atomic.Int32
	runtime, err := newPublicationRuntime(publicationRuntimeOptions{
		DB: fixture.database, Runs: fixture.runs, Manager: fixture.manager,
		Identity:        fixture.parsed.Request.Publisher,
		ExecutorFactory: publicationExecutorFactoryForTest(t, fixture, &recordingPublicationAdapter{}, &factories),
	})
	if err != nil {
		t.Fatal(err)
	}

	before, err := fixture.database.GetPublication(fixture.parsed.PublicationID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Status(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatalf("status projection: %v", err)
	}
	after, err := fixture.database.GetPublication(fixture.parsed.PublicationID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != publication.StatusChecking || !reflect.DeepEqual(before, after) {
		t.Fatalf("status projection changed durable publication: result=%+v before=%+v after=%+v", result, before, after)
	}
	if factories.Load() != 0 || fixture.runs.publicationExecutorActive(result.RunID) {
		t.Fatal("status launched a publication executor")
	}

	_, err = runtime.Authorize(context.Background(), publication.Authorization{
		Decision: publication.DecisionGo, PublicationID: fixture.parsed.PublicationID, Kind: publication.EffectPush,
	})
	if err == nil {
		t.Fatal("authorize without a prepared exact challenge unexpectedly succeeded")
	}
	if factories.Load() != 0 || fixture.runs.publicationExecutorActive(result.RunID) {
		t.Fatal("authorize launched or resumed an executor instead of only persisting through Manager")
	}
}

func TestPublicationRuntimeRecoveryRefusesStoredPublisherIdentityDrift(t *testing.T) {
	fixture := newPublicationRuntimeFixture(t, "publisher-drift")
	if _, err := fixture.manager.Start(context.Background(), fixture.parsed); err != nil {
		t.Fatalf("admit publication through original publisher: %v", err)
	}

	drifted := fixture.parsed.Request.Publisher
	drifted.ExecutableSHA256 = strings.Repeat("a", 64)
	var factories atomic.Int32
	runtime, err := newPublicationRuntime(publicationRuntimeOptions{
		DB: fixture.database, Runs: fixture.runs, Manager: fixture.manager, Identity: drifted,
		ExecutorFactory: publicationExecutorFactoryForTest(t, fixture, &recordingPublicationAdapter{}, &factories),
	})
	if err != nil {
		t.Fatalf("compose drifted daemon runtime: %v", err)
	}
	if _, err := runtime.ResumePublication(context.Background(), fixture.parsed.PublicationID); err == nil {
		t.Fatal("recovery accepted a publication stored by another publisher binary")
	}
	publicationRow, err := fixture.database.GetPublication(fixture.parsed.PublicationID)
	if err != nil || publicationRow == nil {
		t.Fatalf("reload drifted publication: row=%#v err=%v", publicationRow, err)
	}
	if factories.Load() != 0 || fixture.runs.publicationExecutorActive(publicationRow.RunID) {
		t.Fatalf("identity drift reached executor composition: factories=%d", factories.Load())
	}
	run, err := fixture.database.GetRun(publicationRow.RunID)
	if err != nil || run == nil || run.Status.Terminal() {
		t.Fatalf("identity drift destroyed recoverable durable state: run=%#v err=%v", run, err)
	}
}

func TestPublicationRuntimeAuthorizeOnlyReleasesAlreadyParkedProtectedAdapter(t *testing.T) {
	fixture := newPublicationRuntimeFixture(t, "authorize-parked")
	adapter := newRuntimeProtectedAdapter(t, fixture)
	var factories atomic.Int32
	factory := func(_ context.Context, _ string, _ *db.Run, _ *db.Repo) (*publicationExecutorPlan, error) {
		factories.Add(1)
		executor := pipeline.NewExecutor(fixture.database, fixture.paths, &config.Config{}, nil, passingRuntimeSteps(), nil)
		executor.SetPublicationStepAdapter(adapter)
		return &publicationExecutorPlan{Executor: executor}, nil
	}
	runtime, err := newPublicationRuntime(publicationRuntimeOptions{
		DB: fixture.database, Runs: fixture.runs, Manager: fixture.manager,
		Identity: fixture.parsed.Request.Publisher, ExecutorFactory: factory,
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := runtime.Start(context.Background(), fixture.parsed)
	if err != nil {
		t.Fatalf("start protected publication: %v", err)
	}

	waitForPublicationEffect(t, fixture.database, fixture.parsed.PublicationID, db.PublicationEffectPush)
	challenge, err := fixture.manager.PreparePush(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatalf("read internally prepared Push challenge: %v", err)
	}
	if _, err := runtime.Authorize(context.Background(), authorizationForChallenge(challenge)); err != nil {
		t.Fatalf("persist Push authorization: %v", err)
	}
	waitForPublicationEffect(t, fixture.database, fixture.parsed.PublicationID, db.PublicationEffectPR)
	if fixture.push.publishCount() != 1 {
		t.Fatalf("authorized parked adapter Push executions = %d, want one", fixture.push.publishCount())
	}
	if factories.Load() != 1 {
		t.Fatalf("authorize started another executor: factories=%d", factories.Load())
	}
	if !fixture.runs.publicationExecutorActive(started.RunID) {
		t.Fatal("parked publication executor disappeared before PR authorization")
	}

	// Stop the test at the independently gated PR boundary. Shutdown cancels
	// the already parked adapter; Authorize itself never launches or traverses.
	fixture.runs.Shutdown()
}

func TestPublicationRecoveryRestartsPreEffectPushAtDurableWaitBoundary(t *testing.T) {
	for _, authorized := range []bool{false, true} {
		name := "planned"
		if authorized {
			name = "authorized"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newPublicationRuntimeFixture(t, "recover-push-wait-"+name)
			started, err := fixture.manager.Start(context.Background(), fixture.parsed)
			if err != nil {
				t.Fatal(err)
			}
			completePublicationDefensePrefix(t, fixture.database, started.RunID, fixture.parsed.Request.Candidate.CommitSHA)
			pushStep := publicationStep(t, fixture.database, started.RunID, types.StepPush)
			if err := fixture.database.StartStep(pushStep.ID); err != nil {
				t.Fatal(err)
			}
			challenge, err := fixture.manager.PreparePush(context.Background(), fixture.parsed.PublicationID)
			if err != nil {
				t.Fatal(err)
			}
			if authorized {
				if _, err := fixture.manager.Authorize(context.Background(), authorizationForChallenge(challenge)); err != nil {
					t.Fatal(err)
				}
			}

			adapter := newRuntimeProtectedAdapter(t, fixture)
			var factories atomic.Int32
			factory := func(_ context.Context, _ string, _ *db.Run, _ *db.Repo) (*publicationExecutorPlan, error) {
				factories.Add(1)
				executor := pipeline.NewExecutor(fixture.database, fixture.paths, &config.Config{}, nil, passingRuntimeSteps(), nil)
				executor.SetPublicationStepAdapter(adapter)
				return &publicationExecutorPlan{Executor: executor}, nil
			}
			runtime, err := newPublicationRuntime(publicationRuntimeOptions{
				DB: fixture.database, Runs: fixture.runs, Manager: fixture.manager,
				Identity: fixture.parsed.Request.Publisher, ExecutorFactory: factory,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.runs.recoverPublicationRuns(context.Background(), runtime); err != nil {
				t.Fatalf("recover pre-effect Push: %v", err)
			}

			if authorized {
				waitForPublicationEffect(t, fixture.database, fixture.parsed.PublicationID, db.PublicationEffectPR)
				if fixture.push.publishCount() != 1 {
					t.Fatalf("authorized pre-effect recovery Push calls = %d, want one", fixture.push.publishCount())
				}
			} else {
				waitForPublicationStepStatus(t, fixture.database, started.RunID, types.StepPush, types.StepStatusRunning)
				time.Sleep(100 * time.Millisecond)
				if fixture.push.publishCount() != 0 {
					t.Fatalf("planned pre-effect recovery called Push provider %d times", fixture.push.publishCount())
				}
				effect, err := fixture.database.GetPublicationEffect(fixture.parsed.PublicationID, db.PublicationEffectPush)
				if err != nil {
					t.Fatal(err)
				}
				if effect == nil || effect.State != db.PublicationEffectPlanned || effect.EffectStartedAt != nil {
					t.Fatalf("planned Push wait changed durable effect: %#v", effect)
				}
			}
			if factories.Load() != 1 {
				t.Fatalf("recovery executor factories = %d, want one", factories.Load())
			}
			fixture.runs.Shutdown()
		})
	}
}

func TestPublicationRecoveryRestartsPreEffectPRAtDurableWaitBoundary(t *testing.T) {
	for _, authorized := range []bool{false, true} {
		name := "planned"
		if authorized {
			name = "authorized"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newPublicationRuntimeFixture(t, "recover-pr-wait-"+name)
			started, err := fixture.manager.Start(context.Background(), fixture.parsed)
			if err != nil {
				t.Fatal(err)
			}
			completePublicationDefensePrefix(t, fixture.database, started.RunID, fixture.parsed.Request.Candidate.CommitSHA)
			pushChallenge, err := fixture.manager.PreparePush(context.Background(), fixture.parsed.PublicationID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.manager.Authorize(context.Background(), authorizationForChallenge(pushChallenge)); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.manager.ExecutePush(context.Background(), fixture.parsed.PublicationID); err != nil {
				t.Fatal(err)
			}
			pushStep := publicationStep(t, fixture.database, started.RunID, types.StepPush)
			if err := fixture.database.CompleteStep(pushStep.ID, 0, 1, ""); err != nil {
				t.Fatal(err)
			}
			prStep := publicationStep(t, fixture.database, started.RunID, types.StepPR)
			if err := fixture.database.StartStep(prStep.ID); err != nil {
				t.Fatal(err)
			}
			draft, err := fixture.manager.RenderPRDraft(context.Background(), fixture.parsed.PublicationID)
			if err != nil {
				t.Fatal(err)
			}
			prChallenge, err := fixture.manager.PreparePR(context.Background(), fixture.parsed.PublicationID, draft)
			if err != nil {
				t.Fatal(err)
			}
			if authorized {
				if _, err := fixture.manager.Authorize(context.Background(), authorizationForChallenge(prChallenge)); err != nil {
					t.Fatal(err)
				}
			}

			adapter := newRuntimeProtectedAdapter(t, fixture)
			var factories atomic.Int32
			factory := func(_ context.Context, _ string, _ *db.Run, _ *db.Repo) (*publicationExecutorPlan, error) {
				factories.Add(1)
				executor := pipeline.NewExecutor(fixture.database, fixture.paths, &config.Config{}, nil, passingRuntimeSteps(), nil)
				executor.SetPublicationStepAdapter(adapter)
				return &publicationExecutorPlan{Executor: executor}, nil
			}
			runtime, err := newPublicationRuntime(publicationRuntimeOptions{
				DB: fixture.database, Runs: fixture.runs, Manager: fixture.manager,
				Identity: fixture.parsed.Request.Publisher, ExecutorFactory: factory,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.runs.recoverPublicationRuns(context.Background(), runtime); err != nil {
				t.Fatalf("recover pre-effect PR: %v", err)
			}

			if authorized {
				waitPublicationRuntimeIdle(t, fixture.runs)
				if fixture.pr.createCount() != 1 {
					t.Fatalf("authorized pre-effect recovery PR calls = %d, want one", fixture.pr.createCount())
				}
			} else {
				waitForPublicationStepStatus(t, fixture.database, started.RunID, types.StepPR, types.StepStatusRunning)
				time.Sleep(100 * time.Millisecond)
				if fixture.pr.createCount() != 0 {
					t.Fatalf("planned pre-effect recovery called PR provider %d times", fixture.pr.createCount())
				}
				effect, err := fixture.database.GetPublicationEffect(fixture.parsed.PublicationID, db.PublicationEffectPR)
				if err != nil {
					t.Fatal(err)
				}
				if effect == nil || effect.State != db.PublicationEffectPlanned || effect.EffectStartedAt != nil {
					t.Fatalf("planned PR wait changed durable effect: %#v", effect)
				}
				fixture.runs.Shutdown()
			}
			if factories.Load() != 1 {
				t.Fatalf("recovery executor factories = %d, want one", factories.Load())
			}
		})
	}
}

func TestPublicationRuntimeResumeUsesExecutorFirstIncompleteBoundary(t *testing.T) {
	fixture := newPublicationRuntimeFixture(t, "resume-boundary")
	started, err := fixture.manager.Start(context.Background(), fixture.parsed)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []types.StepName{types.StepIntent, types.StepRebase} {
		step := publicationStep(t, fixture.database, started.RunID, name)
		if name == types.StepReview {
			err = fixture.database.CompleteReviewStep(step.ID, started.RunID, fixture.parsed.Request.Candidate.CommitSHA, 0, 1, "")
		} else {
			err = fixture.database.CompleteStep(step.ID, 0, 1, "")
		}
		if err != nil {
			t.Fatalf("complete prefix step %s: %v", name, err)
		}
	}
	adapter := &recordingPublicationAdapter{}
	var factories atomic.Int32
	runtime, err := newPublicationRuntime(publicationRuntimeOptions{
		DB: fixture.database, Runs: fixture.runs, Manager: fixture.manager,
		Identity:        fixture.parsed.Request.Publisher,
		ExecutorFactory: publicationExecutorFactoryForTest(t, fixture, adapter, &factories),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := runtime.ResumePublication(context.Background(), fixture.parsed.PublicationID); err != nil {
		t.Fatalf("resume publication: %v", err)
	}
	waitPublicationRuntimeIdle(t, fixture.runs)
	want := []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI}
	if got := adapter.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("resumed step traversal = %v, want %v", got, want)
	}
	if factories.Load() != 1 {
		t.Fatalf("resume factories = %d, want one", factories.Load())
	}
}

func TestPublicationRuntimeResumeTerminalizesDurableCIThroughExecutor(t *testing.T) {
	for _, test := range []struct {
		name            string
		effect          db.PublicationEffectState
		observationHead string
		wantPublic      publication.ResultStatus
		wantRun         types.RunStatus
		wantStep        types.StepStatus
	}{
		{name: "observed", effect: db.PublicationEffectObserved, observationHead: strings.Repeat("c", 40), wantPublic: publication.StatusReady, wantRun: types.RunCompleted, wantStep: types.StepStatusCompleted},
		{name: "failed", effect: db.PublicationEffectFailed, observationHead: strings.Repeat("c", 40), wantPublic: publication.StatusFailed, wantRun: types.RunFailed, wantStep: types.StepStatusFailed},
		{name: "drift", effect: db.PublicationEffectFailed, observationHead: strings.Repeat("a", 40), wantPublic: publication.StatusDrift, wantRun: types.RunFailed, wantStep: types.StepStatusFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPublicationRuntimeFixture(t, "resume-terminal-ci-"+test.name)
			started, err := fixture.manager.Start(context.Background(), fixture.parsed)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range types.AllSteps() {
				step := publicationStep(t, fixture.database, started.RunID, name)
				if name == types.StepCI {
					if err := fixture.database.StartStep(step.ID); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if name == types.StepReview {
					err = fixture.database.CompleteReviewStep(step.ID, started.RunID, fixture.parsed.Request.Candidate.CommitSHA, 0, 1, "")
				} else {
					err = fixture.database.CompleteStep(step.ID, 0, 1, "")
				}
				if err != nil {
					t.Fatalf("complete prior %s: %v", name, err)
				}
			}
			binding := recoveryEffectBinding(db.PublicationEffectCI)
			if _, err := fixture.database.PlanPublicationEffect(db.PlanPublicationEffectInput{
				PublicationID: fixture.parsed.PublicationID, Kind: db.PublicationEffectCI, Binding: binding,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.database.BeginPublicationEffect(db.BeginPublicationEffectInput{
				PublicationID: fixture.parsed.PublicationID, Kind: db.PublicationEffectCI, Binding: binding,
			}); err != nil {
				t.Fatal(err)
			}
			checkStatus := publication.CICheckFail
			if test.effect == db.PublicationEffectObserved {
				checkStatus = publication.CICheckPass
			}
			observation, err := json.Marshal(publication.CIObservation{
				HeadSHA: test.observationHead,
				Checks:  []publication.CICheck{{Name: "test", HeadSHA: test.observationHead, Status: checkStatus}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.database.ConcludePublicationEffect(db.ConcludePublicationEffectInput{
				PublicationID: fixture.parsed.PublicationID, Kind: db.PublicationEffectCI, Binding: binding,
				State: test.effect, Observation: observation,
			}); err != nil {
				t.Fatal(err)
			}

			adapter := &recordingPublicationAdapter{}
			var factories atomic.Int32
			runtime, err := newPublicationRuntime(publicationRuntimeOptions{
				DB: fixture.database, Runs: fixture.runs, Manager: fixture.manager,
				Identity:        fixture.parsed.Request.Publisher,
				ExecutorFactory: publicationExecutorFactoryForTest(t, fixture, adapter, &factories),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runtime.ResumePublication(context.Background(), fixture.parsed.PublicationID); err != nil {
				t.Fatalf("resume terminal CI: %v", err)
			}
			waitPublicationRuntimeIdle(t, fixture.runs)
			if calls := adapter.snapshot(); len(calls) != 0 {
				t.Fatalf("terminal CI recovery called adapter again: %v", calls)
			}
			if factories.Load() != 1 {
				t.Fatalf("executor factories = %d, want one", factories.Load())
			}
			run, err := fixture.database.GetRun(started.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != test.wantRun {
				t.Fatalf("Run status = %s, want %s", run.Status, test.wantRun)
			}
			projected, err := fixture.manager.Status(context.Background(), fixture.parsed.PublicationID)
			if err != nil {
				t.Fatal(err)
			}
			if projected.Status != test.wantPublic {
				t.Fatalf("public terminal status = %s, want %s", projected.Status, test.wantPublic)
			}
			ci := publicationStep(t, fixture.database, started.RunID, types.StepCI)
			if ci.Status != test.wantStep {
				t.Fatalf("CI status = %s, want %s", ci.Status, test.wantStep)
			}
		})
	}
}

func TestPublicationRuntimeRecoveryReconcilesStartedPushBeforeExecutorRemainder(t *testing.T) {
	fixture := newPublicationRuntimeFixture(t, "recover-push")
	started, err := fixture.manager.Start(context.Background(), fixture.parsed)
	if err != nil {
		t.Fatal(err)
	}
	completePublicationDefensePrefix(t, fixture.database, started.RunID, fixture.parsed.Request.Candidate.CommitSHA)
	pushStep := publicationStep(t, fixture.database, started.RunID, types.StepPush)
	if err := fixture.database.StartStep(pushStep.ID); err != nil {
		t.Fatal(err)
	}
	challenge, err := fixture.manager.PreparePush(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatalf("prepare push: %v", err)
	}
	if _, err := fixture.manager.Authorize(context.Background(), authorizationForChallenge(challenge)); err != nil {
		t.Fatalf("authorize push: %v", err)
	}
	effect, err := fixture.database.GetPublicationEffect(fixture.parsed.PublicationID, db.PublicationEffectPush)
	if err != nil || effect == nil || effect.DecisionDigest == nil {
		t.Fatalf("load authorized Push effect: effect=%#v err=%v", effect, err)
	}
	if _, err := fixture.database.BeginPublicationEffect(db.BeginPublicationEffectInput{
		PublicationID:  fixture.parsed.PublicationID,
		Kind:           db.PublicationEffectPush,
		Binding:        effect.Binding,
		DecisionDigest: *effect.DecisionDigest,
	}); err != nil {
		t.Fatalf("persist Push STARTED before simulated crash: %v", err)
	}
	if err := fixture.push.PublishExact(context.Background(), publication.PushEffectRequest{
		PublicationID:  fixture.parsed.PublicationID,
		RepositoryID:   fixture.parsed.Request.Candidate.RepositoryID,
		CommitSHA:      challenge.CommitSHA,
		RemoteIdentity: challenge.RemoteIdentity,
		DestinationRef: challenge.DestinationRef,
		EffectDigest:   challenge.EffectDigest,
	}); err != nil {
		t.Fatalf("apply Push immediately before simulated control-plane crash: %v", err)
	}
	if fixture.push.publishCount() != 1 {
		t.Fatalf("push executions before recovery = %d, want one", fixture.push.publishCount())
	}

	adapter := &recordingPublicationAdapter{}
	var factories atomic.Int32
	runtime, err := newPublicationRuntime(publicationRuntimeOptions{
		DB: fixture.database, Runs: fixture.runs, Manager: fixture.manager,
		Identity:        fixture.parsed.Request.Publisher,
		ExecutorFactory: publicationExecutorFactoryForTest(t, fixture, adapter, &factories),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.RecoverEffect(context.Background(), fixture.parsed.PublicationID, publication.EffectPush)
	if err != nil {
		t.Fatalf("recover started push: %v", err)
	}
	if result.Status != publication.StatusReadyForPR {
		t.Fatalf("reconciled status = %s, want %s", result.Status, publication.StatusReadyForPR)
	}
	waitPublicationRuntimeIdle(t, fixture.runs)
	if fixture.push.publishCount() != 1 {
		t.Fatalf("recovery replayed push: executions=%d", fixture.push.publishCount())
	}
	want := []types.StepName{types.StepPR, types.StepCI}
	if got := adapter.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("post-reconcile traversal = %v, want %v", got, want)
	}
	recoveredPush := publicationStep(t, fixture.database, started.RunID, types.StepPush)
	if recoveredPush.Status != types.StepStatusCompleted {
		t.Fatalf("reconciled Push step = %s, want completed", recoveredPush.Status)
	}
}

func TestPublicationRuntimeRecoveryReconcilesStartedPRBeforeExecutorRemainder(t *testing.T) {
	fixture := newPublicationRuntimeFixture(t, "recover-pr")
	started, err := fixture.manager.Start(context.Background(), fixture.parsed)
	if err != nil {
		t.Fatal(err)
	}
	completePublicationDefensePrefix(t, fixture.database, started.RunID, fixture.parsed.Request.Candidate.CommitSHA)
	pushChallenge, err := fixture.manager.PreparePush(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.manager.Authorize(context.Background(), authorizationForChallenge(pushChallenge)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.manager.ExecutePush(context.Background(), fixture.parsed.PublicationID); err != nil {
		t.Fatal(err)
	}
	pushStep := publicationStep(t, fixture.database, started.RunID, types.StepPush)
	if err := fixture.database.CompleteStep(pushStep.ID, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	prStep := publicationStep(t, fixture.database, started.RunID, types.StepPR)
	if err := fixture.database.StartStep(prStep.ID); err != nil {
		t.Fatal(err)
	}
	challenge, err := fixture.manager.PreparePR(context.Background(), fixture.parsed.PublicationID, []byte("exact publication draft\n"))
	if err != nil {
		t.Fatalf("prepare PR: %v", err)
	}
	if _, err := fixture.manager.Authorize(context.Background(), authorizationForChallenge(challenge)); err != nil {
		t.Fatalf("authorize PR: %v", err)
	}
	effect, err := fixture.database.GetPublicationEffect(fixture.parsed.PublicationID, db.PublicationEffectPR)
	if err != nil || effect == nil || effect.DecisionDigest == nil {
		t.Fatalf("load authorized PR effect: effect=%#v err=%v", effect, err)
	}
	if _, err := fixture.database.BeginPublicationEffect(db.BeginPublicationEffectInput{
		PublicationID:  fixture.parsed.PublicationID,
		Kind:           db.PublicationEffectPR,
		Binding:        effect.Binding,
		DecisionDigest: *effect.DecisionDigest,
	}); err != nil {
		t.Fatalf("persist PR STARTED before simulated crash: %v", err)
	}
	if err := fixture.pr.CreateExact(context.Background(), publication.PREffectRequest{
		PublicationID: fixture.parsed.PublicationID,
		RepositoryID:  fixture.parsed.Request.Candidate.RepositoryID,
		BaseRef:       challenge.BaseRef,
		HeadRef:       challenge.HeadRef,
		CommitSHA:     challenge.CommitSHA,
		Marker:        challenge.Marker,
		Draft:         []byte(challenge.PreparedDraft),
		DraftSHA256:   challenge.DraftSHA256,
		EffectDigest:  challenge.EffectDigest,
	}); err != nil {
		t.Fatalf("apply PR immediately before simulated control-plane crash: %v", err)
	}
	if fixture.pr.createCount() != 1 {
		t.Fatalf("PR creates before recovery = %d, want one", fixture.pr.createCount())
	}

	adapter := &recordingPublicationAdapter{}
	var factories atomic.Int32
	runtime, err := newPublicationRuntime(publicationRuntimeOptions{
		DB: fixture.database, Runs: fixture.runs, Manager: fixture.manager,
		Identity:        fixture.parsed.Request.Publisher,
		ExecutorFactory: publicationExecutorFactoryForTest(t, fixture, adapter, &factories),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.RecoverEffect(context.Background(), fixture.parsed.PublicationID, publication.EffectPR)
	if err != nil {
		t.Fatalf("recover started PR: %v", err)
	}
	if result.Status != publication.StatusCIObserving {
		t.Fatalf("reconciled status = %s, want %s", result.Status, publication.StatusCIObserving)
	}
	waitPublicationRuntimeIdle(t, fixture.runs)
	if fixture.pr.createCount() != 1 {
		t.Fatalf("recovery replayed PR: creates=%d", fixture.pr.createCount())
	}
	if got := adapter.snapshot(); !reflect.DeepEqual(got, []types.StepName{types.StepCI}) {
		t.Fatalf("post-PR reconcile traversal = %v, want [ci]", got)
	}
	recoveredPR := publicationStep(t, fixture.database, started.RunID, types.StepPR)
	if recoveredPR.Status != types.StepStatusCompleted {
		t.Fatalf("reconciled PR step = %s, want completed", recoveredPR.Status)
	}
}

func TestPublicationRecoveryResetsInterruptedCIAsRepeatableReadOnlyWork(t *testing.T) {
	fixture := newPublicationRuntimeFixture(t, "recover-ci")
	started, err := fixture.manager.Start(context.Background(), fixture.parsed)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range types.AllSteps()[:len(types.AllSteps())-1] {
		step := publicationStep(t, fixture.database, started.RunID, name)
		if name == types.StepReview {
			err = fixture.database.CompleteReviewStep(step.ID, started.RunID, fixture.parsed.Request.Candidate.CommitSHA, 0, 1, "")
		} else {
			err = fixture.database.CompleteStep(step.ID, 0, 1, "")
		}
		if err != nil {
			t.Fatalf("complete %s: %v", name, err)
		}
	}
	ciStep := publicationStep(t, fixture.database, started.RunID, types.StepCI)
	if err := fixture.database.StartStep(ciStep.ID); err != nil {
		t.Fatal(err)
	}
	service := &fakePublicationRecoveryService{resumeCheck: func(string) error {
		fresh := publicationStep(t, fixture.database, started.RunID, types.StepCI)
		if fresh.Status != types.StepStatusPending || fresh.StartedAt != nil {
			return errors.New("interrupted CI was not reset to a fresh pending boundary")
		}
		return nil
	}}
	if _, err := fixture.runs.recoverPublicationRuns(context.Background(), service); err != nil {
		t.Fatalf("recover interrupted CI: %v", err)
	}
}

func TestPublicationRecoveryResetsUnstartedPushAndPRWithoutConsumingDecision(t *testing.T) {
	for _, kind := range []publication.EffectKind{publication.EffectPush, publication.EffectPR} {
		for _, authorized := range []bool{false, true} {
			name := string(kind) + "/planned"
			if authorized {
				name = string(kind) + "/authorized"
			}
			t.Run(name, func(t *testing.T) {
				fixture := newPublicationRuntimeFixture(t, "recover-wait-"+strings.ReplaceAll(name, "/", "-"))
				started, err := fixture.manager.Start(context.Background(), fixture.parsed)
				if err != nil {
					t.Fatal(err)
				}
				completePublicationDefensePrefix(t, fixture.database, started.RunID, fixture.parsed.Request.Candidate.CommitSHA)

				var challenge publication.EffectChallenge
				var stepName types.StepName
				switch kind {
				case publication.EffectPush:
					stepName = types.StepPush
					step := publicationStep(t, fixture.database, started.RunID, stepName)
					if err := fixture.database.StartStep(step.ID); err != nil {
						t.Fatal(err)
					}
					challenge, err = fixture.manager.PreparePush(context.Background(), fixture.parsed.PublicationID)
				case publication.EffectPR:
					pushChallenge, prepareErr := fixture.manager.PreparePush(context.Background(), fixture.parsed.PublicationID)
					if prepareErr != nil {
						t.Fatal(prepareErr)
					}
					if _, err := fixture.manager.Authorize(context.Background(), authorizationForChallenge(pushChallenge)); err != nil {
						t.Fatal(err)
					}
					if _, err := fixture.manager.ExecutePush(context.Background(), fixture.parsed.PublicationID); err != nil {
						t.Fatal(err)
					}
					pushStep := publicationStep(t, fixture.database, started.RunID, types.StepPush)
					if err := fixture.database.CompleteStep(pushStep.ID, 0, 1, ""); err != nil {
						t.Fatal(err)
					}
					stepName = types.StepPR
					step := publicationStep(t, fixture.database, started.RunID, stepName)
					if err := fixture.database.StartStep(step.ID); err != nil {
						t.Fatal(err)
					}
					challenge, err = fixture.manager.PreparePR(context.Background(), fixture.parsed.PublicationID, []byte("exact draft\n"))
				}
				if err != nil {
					t.Fatalf("prepare %s: %v", kind, err)
				}
				if authorized {
					if _, err := fixture.manager.Authorize(context.Background(), authorizationForChallenge(challenge)); err != nil {
						t.Fatalf("authorize %s: %v", kind, err)
					}
				}

				if err := fixture.runs.resetInterruptedPublicationDefense(started.RunID); err != nil {
					t.Fatalf("reset unstarted %s: %v", kind, err)
				}
				fresh := publicationStep(t, fixture.database, started.RunID, stepName)
				if fresh.Status != types.StepStatusPending || fresh.StartedAt != nil {
					t.Fatalf("unstarted %s step after recovery = %#v, want fresh pending", kind, fresh)
				}
				dbKind := db.PublicationEffectPush
				wantState := db.PublicationEffectPlanned
				if kind == publication.EffectPR {
					dbKind = db.PublicationEffectPR
				}
				if authorized {
					wantState = db.PublicationEffectAuthorized
				}
				effect, err := fixture.database.GetPublicationEffect(fixture.parsed.PublicationID, dbKind)
				if err != nil {
					t.Fatal(err)
				}
				if effect == nil || effect.State != wantState || effect.EffectStartedAt != nil {
					t.Fatalf("%s decision was consumed or changed during recovery: %#v", kind, effect)
				}
			})
		}
	}
}

func completePublicationDefensePrefix(t *testing.T, database *db.DB, runID, headSHA string) {
	t.Helper()
	for _, name := range []types.StepName{types.StepIntent, types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
		step := publicationStep(t, database, runID, name)
		var err error
		if name == types.StepReview {
			err = database.CompleteReviewStep(step.ID, runID, headSHA, 0, 1, "")
		} else {
			err = database.CompleteStep(step.ID, 0, 1, "")
		}
		if err != nil {
			t.Fatalf("complete %s: %v", name, err)
		}
	}
}

func authorizationForChallenge(challenge publication.EffectChallenge) publication.Authorization {
	return publication.Authorization{
		Decision:       publication.DecisionGo,
		PublicationID:  challenge.PublicationID,
		Kind:           challenge.Kind,
		Attempt:        challenge.Attempt,
		CommitSHA:      challenge.CommitSHA,
		RemoteIdentity: challenge.RemoteIdentity,
		DestinationRef: challenge.DestinationRef,
		BaseRef:        challenge.BaseRef,
		HeadRef:        challenge.HeadRef,
		DraftSHA256:    challenge.DraftSHA256,
		EffectDigest:   challenge.EffectDigest,
		DecisionDigest: challenge.DecisionDigest,
	}
}

func newRuntimeProtectedAdapter(t *testing.T, fixture publicationRuntimeFixture) pipeline.PublicationStepAdapter {
	t.Helper()
	candidate := &runtimeCandidatePort{root: t.TempDir(), guard: runtimeCandidateGuard{}}
	adapter, err := pipelinesteps.NewFactoryPublicationStepAdapter(pipelinesteps.FactoryPublicationStepAdapterOptions{
		PublicationID: fixture.parsed.PublicationID,
		Manager:       fixture.manager,
		Candidate:     candidate,
		Freshness:     runtimeFreshnessPort{},
		RenderPRDraft: fixture.manager.RenderPRDraft,
	})
	if err != nil {
		t.Fatalf("compose protected adapter: %v", err)
	}
	return adapter
}

func waitPublicationRuntimeIdle(t *testing.T, manager *RunManager) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		manager.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publication runtime did not become idle")
	}
}

func waitForPublicationEffect(t *testing.T, database *db.DB, publicationID string, kind db.PublicationEffectKind) *db.PublicationEffect {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		effect, err := database.GetPublicationEffect(publicationID, kind)
		if err != nil {
			t.Fatalf("read %s effect: %v", kind, err)
		}
		if effect != nil {
			return effect
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("publication %s effect %s was not prepared", publicationID, kind)
	return nil
}

func waitForPublicationStepStatus(t *testing.T, database *db.DB, runID string, name types.StepName, status types.StepStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		step := publicationStep(t, database, runID, name)
		if step.Status == status {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("publication step %s did not reach %s", name, status)
}
