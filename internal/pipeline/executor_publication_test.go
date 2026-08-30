package pipeline

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/publication"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	publicationExecutorHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	publicationExecutorTree = "1111111111111111111111111111111111111111"
)

// publicationStepAdapterFake is the one injected policy seam these tests
// require from Executor. It deliberately has no scheduler, retry loop, resume
// loop, counter, or generic approval surface: Executor remains the sole owner
// of the ordered nine-step traversal.
type publicationStepAdapterFake struct {
	mu sync.Mutex

	calls          []types.StepName
	intentCalls    int
	guardCalls     []types.StepName
	pushCalls      int
	prCalls        int
	ciObservedHead string
	fixingSeen     bool

	outcomes map[types.StepName]*StepOutcome

	gateEffects bool
	pushReached chan struct{}
	pushGO      chan struct{}
	prReached   chan struct{}
	prGO        chan struct{}
}

func newPublicationStepAdapterFake(gateEffects bool) *publicationStepAdapterFake {
	return &publicationStepAdapterFake{
		outcomes:    make(map[types.StepName]*StepOutcome),
		gateEffects: gateEffects,
		pushReached: make(chan struct{}),
		pushGO:      make(chan struct{}),
		prReached:   make(chan struct{}),
		prGO:        make(chan struct{}),
	}
}

// ExecutePublicationStep is intentionally the adapter's only execution
// method. The production interface should match this shape so the run kind can
// select publication semantics without introducing another executor.
func (f *publicationStepAdapterFake) ExecutePublicationStep(sctx *StepContext, step Step) (*StepOutcome, error) {
	name := step.Name()
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.fixingSeen = f.fixingSeen || sctx.Fixing
	switch name {
	case types.StepIntent:
		f.intentCalls++
	case types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint:
		f.guardCalls = append(f.guardCalls, name)
	case types.StepCI:
		f.ciObservedHead = sctx.Run.HeadSHA
	}
	outcome := f.outcomes[name]
	f.mu.Unlock()

	switch name {
	case types.StepPush:
		if f.gateEffects {
			close(f.pushReached)
			select {
			case <-sctx.Ctx.Done():
				return nil, sctx.Ctx.Err()
			case <-f.pushGO:
			}
		}
		f.mu.Lock()
		f.pushCalls++
		f.mu.Unlock()
	case types.StepPR:
		if f.gateEffects {
			close(f.prReached)
			select {
			case <-sctx.Ctx.Done():
				return nil, sctx.Ctx.Err()
			case <-f.prGO:
			}
		}
		f.mu.Lock()
		f.prCalls++
		f.mu.Unlock()
	}

	if outcome != nil {
		copy := *outcome
		return &copy, nil
	}
	return &StepOutcome{}, nil
}

func (f *publicationStepAdapterFake) snapshot() (calls, guards []types.StepName, intent, push, pr int, ciHead string, fixing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]types.StepName(nil), f.calls...),
		append([]types.StepName(nil), f.guardCalls...),
		f.intentCalls, f.pushCalls, f.prCalls, f.ciObservedHead, f.fixingSeen
}

type publicationExecutorFixture struct {
	database    *db.DB
	paths       *paths.Paths
	run         *db.Run
	repo        *db.Repo
	publication *db.Publication
	workDir     string
}

type publicationProjectionCandidate struct{}

func (publicationProjectionCandidate) Inspect(context.Context, string, types.StepName) (publication.CandidateSnapshot, error) {
	return publication.CandidateSnapshot{}, nil
}

type publicationProjectionPush struct{}

func (publicationProjectionPush) PublishExact(context.Context, publication.PushEffectRequest) error {
	return nil
}
func (publicationProjectionPush) ObserveExact(context.Context, publication.PushEffectRequest) (publication.PushObservation, error) {
	return publication.PushObservation{}, nil
}

type publicationProjectionPR struct{}

func (publicationProjectionPR) CreateExact(context.Context, publication.PREffectRequest) error {
	return nil
}
func (publicationProjectionPR) FindExact(context.Context, publication.PRReconcileQuery) ([]publication.PRObservation, error) {
	return nil, nil
}

type publicationProjectionCI struct{}

func (publicationProjectionCI) ObserveExact(context.Context, publication.CIQuery) (publication.CIObservation, error) {
	return publication.CIObservation{}, nil
}

func newPublicationExecutorFixture(t *testing.T) *publicationExecutorFixture {
	t.Helper()
	root := t.TempDir()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure paths: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	repo, err := database.InsertRepoWithID(
		"abc123def456",
		t.TempDir(),
		"https://github.com/example/project.git",
		"main",
	)
	if err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	raw := []byte(`{"protocol":"factory-publication-v1","test":"executor-integration"}`)
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	publication, run, created, err := database.CreateOrGetPublication(db.CreatePublicationInput{
		PublicationID:    digest,
		CanonicalRequest: raw,
		RepoID:           repo.ID,
		CandidateRef:     "refs/heads/feature/publication",
		BaseRef:          "refs/heads/main",
		BaseSHA:          strings.Repeat("b", 40),
		HeadSHA:          publicationExecutorHead,
		TreeSHA:          publicationExecutorTree,
	})
	if err != nil {
		t.Fatalf("create publication run: %v", err)
	}
	if !created {
		t.Fatal("fresh fixture reconciled an existing publication")
	}
	return &publicationExecutorFixture{
		database:    database,
		paths:       p,
		run:         run,
		repo:        repo,
		publication: publication,
		workDir:     t.TempDir(),
	}
}

func newGitBoundPublicationExecutorFixture(t *testing.T) *publicationExecutorFixture {
	t.Helper()
	root := t.TempDir()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	workDir := t.TempDir()
	initGitRepo(t, workDir)
	head, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := git.Run(context.Background(), workDir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepoWithID("gitbound123", workDir, "https://github.com/example/project.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"protocol":"factory-publication-v1","test":"terminal-head-binding"}`)
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	publicationRow, run, created, err := database.CreateOrGetPublication(db.CreatePublicationInput{
		PublicationID: digest, CanonicalRequest: raw, RepoID: repo.ID,
		CandidateRef: "refs/heads/feature/publication", BaseRef: "refs/heads/main",
		BaseSHA: head, HeadSHA: head, TreeSHA: tree,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("fresh fixture reconciled an existing publication")
	}
	return &publicationExecutorFixture{
		database: database, paths: p, run: run, repo: repo,
		publication: publicationRow, workDir: workDir,
	}
}

func publicationExecutorSteps() ([]Step, []*mockStep) {
	names := types.AllSteps()
	steps := make([]Step, 0, len(names))
	standard := make([]*mockStep, 0, len(names))
	for _, name := range names {
		step := newPassStep(name)
		steps = append(steps, step)
		standard = append(standard, step)
	}
	return steps, standard
}

func stepIDs(steps []*db.StepResult) []string {
	ids := make([]string, 0, len(steps))
	for _, step := range steps {
		ids = append(ids, step.ID)
	}
	return ids
}

func TestExecutorFactoryPublicationUsesSeededRowsAndOneOrderedLoop(t *testing.T) {
	fixture := newPublicationExecutorFixture(t)
	seeded, err := fixture.database.GetStepsByRun(fixture.run.ID)
	if err != nil {
		t.Fatalf("read seeded steps: %v", err)
	}
	if len(seeded) != len(types.AllSteps()) {
		t.Fatalf("seeded steps = %d, want %d", len(seeded), len(types.AllSteps()))
	}
	seededIDs := stepIDs(seeded)

	steps, standardSteps := publicationExecutorSteps()
	adapter := newPublicationStepAdapterFake(false)
	executor := NewExecutor(fixture.database, fixture.paths, nil, nil, steps, nil)
	executor.SetPublicationStepAdapter(adapter)

	if err := executor.Execute(context.Background(), fixture.run, fixture.repo, fixture.workDir); err != nil {
		t.Fatalf("execute publication: %v", err)
	}

	calls, guards, intentCalls, pushCalls, prCalls, ciHead, fixingSeen := adapter.snapshot()
	if !reflect.DeepEqual(calls, types.AllSteps()) {
		t.Fatalf("publication order = %v, want %v", calls, types.AllSteps())
	}
	if intentCalls != 1 {
		t.Fatalf("intent validation calls = %d, want 1", intentCalls)
	}
	wantGuards := []types.StepName{
		types.StepRebase,
		types.StepReview,
		types.StepTest,
		types.StepDocument,
		types.StepLint,
	}
	if !reflect.DeepEqual(guards, wantGuards) {
		t.Fatalf("guarded defense calls = %v, want %v", guards, wantGuards)
	}
	if pushCalls != 1 || prCalls != 1 {
		t.Fatalf("effect adapter calls push=%d pr=%d, want 1 each", pushCalls, prCalls)
	}
	if ciHead != publicationExecutorHead {
		t.Fatalf("CI observed head = %q, want exact H %q", ciHead, publicationExecutorHead)
	}
	if fixingSeen {
		t.Fatal("publication adapter was invoked in fixing mode")
	}
	for _, step := range standardSteps {
		if got := step.callCount(); got != 0 {
			t.Errorf("standard %s step executed %d times; publication adapter must own the profile", step.Name(), got)
		}
	}

	completed, err := fixture.database.GetStepsByRun(fixture.run.ID)
	if err != nil {
		t.Fatalf("read completed steps: %v", err)
	}
	if len(completed) != len(types.AllSteps()) {
		t.Fatalf("step rows after execution = %d, want the same seeded 9", len(completed))
	}
	if got := stepIDs(completed); !reflect.DeepEqual(got, seededIDs) {
		t.Fatalf("Executor replaced or duplicated seeded rows: got %v, want %v", got, seededIDs)
	}
	for _, step := range completed {
		if step.Status != types.StepStatusCompleted {
			t.Errorf("%s status = %s, want completed", step.StepName, step.Status)
		}
		rounds, err := fixture.database.GetRoundsByStep(step.ID)
		if err != nil {
			t.Fatalf("read %s rounds: %v", step.StepName, err)
		}
		if len(rounds) != 1 || rounds[0].Round != 1 {
			t.Errorf("%s rounds = %d, want exactly one initial execution", step.StepName, len(rounds))
		}
	}
}

func TestExecutorFactoryPublicationSuppressesRemoteStepTelemetry(t *testing.T) {
	fixture := newPublicationExecutorFixture(t)
	steps, _ := publicationExecutorSteps()
	adapter := newPublicationStepAdapterFake(false)
	adapter.outcomes[types.StepIntent] = &StepOutcome{ExitCode: 1}
	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	executor := NewExecutor(fixture.database, fixture.paths, nil, nil, steps, nil)
	executor.SetPublicationStepAdapter(adapter)
	if err := executor.Execute(context.Background(), fixture.run, fixture.repo, fixture.workDir); err == nil {
		t.Fatal("publication defense failure unexpectedly succeeded")
	}
	if event := recorder.find("step", "", nil); event != nil {
		t.Fatalf("publication emitted remote step telemetry: %#v", event)
	}
}

func TestExecutorFactoryPublicationFailsClosedWithoutAdapter(t *testing.T) {
	fixture := newPublicationExecutorFixture(t)
	steps, standardSteps := publicationExecutorSteps()
	executor := NewExecutor(fixture.database, fixture.paths, nil, nil, steps, nil)

	err := executor.Execute(context.Background(), fixture.run, fixture.repo, fixture.workDir)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "publication") {
		t.Fatalf("Execute() error = %v, want fail-closed missing publication adapter", err)
	}
	for _, step := range standardSteps {
		if step.callCount() != 0 {
			t.Fatalf("standard step %s ran without publication adapter", step.Name())
		}
	}
}

func seedInterruptedPublicationCI(t *testing.T, fixture *publicationExecutorFixture, state db.PublicationEffectState) *db.StepResult {
	t.Helper()
	if err := fixture.database.UpdateRunStatus(fixture.run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	fixture.run.Status = types.RunRunning
	seeded, err := fixture.database.GetStepsByRun(fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range seeded {
		if step.StepName == types.StepCI {
			if err := fixture.database.StartStep(step.ID); err != nil {
				t.Fatalf("start interrupted CI: %v", err)
			}
			continue
		}
		if err := fixture.database.CompleteStep(step.ID, 0, 1, ""); err != nil {
			t.Fatalf("complete prior %s: %v", step.StepName, err)
		}
	}
	ciStep := seeded[len(seeded)-1]
	binding := db.PublicationEffectBinding{
		CandidateSHA:   fixture.publication.HeadSHA,
		RemoteIdentity: "github.com/example/project",
		BaseRef:        fixture.publication.BaseRef,
		HeadRef:        fixture.publication.CandidateRef,
		EffectDigest:   "terminal-ci-effect-" + string(state),
	}
	if _, err := fixture.database.PlanPublicationEffect(db.PlanPublicationEffectInput{
		PublicationID: fixture.publication.PublicationID,
		Kind:          db.PublicationEffectCI,
		Binding:       binding,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.database.BeginPublicationEffect(db.BeginPublicationEffectInput{
		PublicationID: fixture.publication.PublicationID,
		Kind:          db.PublicationEffectCI,
		Binding:       binding,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.database.ConcludePublicationEffect(db.ConcludePublicationEffectInput{
		PublicationID: fixture.publication.PublicationID,
		Kind:          db.PublicationEffectCI,
		Binding:       binding,
		State:         state,
		Observation:   []byte(`{"head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","checks":["terminal"]}`),
	}); err != nil {
		t.Fatal(err)
	}
	return ciStep
}

func TestResumePublicationCompletesObservedCIWithoutReobservation(t *testing.T) {
	fixture := newPublicationExecutorFixture(t)
	ciStep := seedInterruptedPublicationCI(t, fixture, db.PublicationEffectObserved)

	steps, _ := publicationExecutorSteps()
	adapter := newPublicationStepAdapterFake(false)
	executor := NewExecutor(fixture.database, fixture.paths, nil, nil, steps, nil)
	executor.SetPublicationStepAdapter(adapter)
	if err := executor.ResumePublication(context.Background(), fixture.run, fixture.repo, fixture.workDir); err != nil {
		t.Fatalf("resume observed CI: %v", err)
	}

	calls, _, _, _, _, ciHead, _ := adapter.snapshot()
	if len(calls) != 0 || ciHead != "" {
		t.Fatalf("resume re-observed CI through adapter: calls=%v head=%q", calls, ciHead)
	}
	gotCI, err := fixture.database.GetStepResult(ciStep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotCI.Status != types.StepStatusCompleted {
		t.Fatalf("reconciled CI status = %s, want completed", gotCI.Status)
	}
	gotRun, err := fixture.database.GetRun(fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRun.Status != types.RunCompleted {
		t.Fatalf("resumed Run status = %s, want completed", gotRun.Status)
	}
}

func TestResumePublicationFailsTerminalCIThroughExecutorWithoutReobservation(t *testing.T) {
	for _, state := range []db.PublicationEffectState{db.PublicationEffectFailed, db.PublicationEffectUnknown} {
		t.Run(string(state), func(t *testing.T) {
			fixture := newPublicationExecutorFixture(t)
			ciStep := seedInterruptedPublicationCI(t, fixture, state)
			steps, _ := publicationExecutorSteps()
			adapter := newPublicationStepAdapterFake(false)
			executor := NewExecutor(fixture.database, fixture.paths, nil, nil, steps, nil)
			executor.SetPublicationStepAdapter(adapter)

			if err := executor.ResumePublication(context.Background(), fixture.run, fixture.repo, fixture.workDir); err == nil {
				t.Fatalf("resume accepted terminal CI effect %s", state)
			}
			calls, _, _, _, _, ciHead, _ := adapter.snapshot()
			if len(calls) != 0 || ciHead != "" {
				t.Fatalf("resume re-observed terminal CI through adapter: calls=%v head=%q", calls, ciHead)
			}
			gotRun, err := fixture.database.GetRun(fixture.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotRun.Status != types.RunFailed {
				t.Fatalf("terminal CI Run status = %s, want failed by Executor", gotRun.Status)
			}
			gotCI, err := fixture.database.GetStepResult(ciStep.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotCI.Status != types.StepStatusFailed {
				t.Fatalf("terminal CI step status = %s, want failed by Executor", gotCI.Status)
			}
		})
	}
}

func TestExecutorFactoryPublicationRejectsGenericPipelineControls(t *testing.T) {
	t.Run("configured skip", func(t *testing.T) {
		fixture := newPublicationExecutorFixture(t)
		steps, _ := publicationExecutorSteps()
		adapter := newPublicationStepAdapterFake(false)
		executor := NewExecutor(fixture.database, fixture.paths, nil, nil, steps, nil)
		executor.SetPublicationStepAdapter(adapter)
		executor.SetSkippedSteps([]types.StepName{types.StepReview})

		if err := executor.Execute(context.Background(), fixture.run, fixture.repo, fixture.workDir); err == nil {
			t.Fatal("publication run accepted SetSkippedSteps")
		}
		calls, _, _, _, _, _, _ := adapter.snapshot()
		if len(calls) != 0 {
			t.Fatalf("publication adapter ran after configured skip: %v", calls)
		}
	})

	cases := []struct {
		name    string
		outcome *StepOutcome
	}{
		{name: "step skip", outcome: &StepOutcome{Skipped: true}},
		{name: "skip remainder", outcome: &StepOutcome{SkipRemaining: true}},
		{name: "restart", outcome: &StepOutcome{RestartFrom: types.StepIntent}},
		{name: "generic approval", outcome: &StepOutcome{NeedsApproval: true}},
		{
			name:    "ask-user quality approval",
			outcome: &StepOutcome{Findings: `{"findings":[{"id":"f1","severity":"error","description":"quality failed","action":"ask-user"}]}`},
		},
		{
			name: "auto-fix",
			outcome: &StepOutcome{
				AutoFixable: true,
				Findings:    `{"findings":[{"id":"f1","severity":"error","description":"quality failed","action":"auto-fix"}]}`,
			},
		},
		{name: "fix summary", outcome: &StepOutcome{FixSummary: "changed the candidate"}},
		{name: "nonzero exit", outcome: &StepOutcome{ExitCode: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPublicationExecutorFixture(t)
			steps, _ := publicationExecutorSteps()
			adapter := newPublicationStepAdapterFake(false)
			adapter.outcomes[types.StepIntent] = tc.outcome
			executor := NewExecutor(fixture.database, fixture.paths, nil, nil, steps, nil)
			executor.SetPublicationStepAdapter(adapter)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := executor.Execute(ctx, fixture.run, fixture.repo, fixture.workDir); err == nil {
				t.Fatalf("publication run accepted forbidden outcome %#v", tc.outcome)
			} else if ctx.Err() != nil {
				t.Fatalf("forbidden outcome entered a generic wait/loop instead of failing closed: %v", err)
			}
			calls, _, _, _, _, _, fixingSeen := adapter.snapshot()
			if len(calls) != 1 || calls[0] != types.StepIntent {
				t.Fatalf("calls after forbidden outcome = %v, want [intent]", calls)
			}
			if fixingSeen {
				t.Fatal("forbidden outcome entered fixing mode")
			}
		})
	}
}

func TestExecutorFactoryPublicationPushAndPRResumeOnlyFromExternalAuthorization(t *testing.T) {
	fixture := newPublicationExecutorFixture(t)
	steps, _ := publicationExecutorSteps()
	adapter := newPublicationStepAdapterFake(true)
	executor := NewExecutor(fixture.database, fixture.paths, nil, nil, steps, nil)
	executor.SetPublicationStepAdapter(adapter)

	done, _ := startExecutor(t, executor, fixture.run, fixture.repo, fixture.workDir)

	select {
	case <-adapter.pushReached:
	case <-time.After(5 * time.Second):
		t.Fatal("publication never reached pre-Push authorization park")
	}
	_, _, _, pushCalls, _, _, _ := adapter.snapshot()
	if pushCalls != 0 {
		t.Fatalf("Push port calls before external authorization = %d, want 0", pushCalls)
	}
	if err := executor.Respond(types.StepPush, types.ActionApprove, nil); err == nil {
		t.Fatal("generic AXI approve authorized publication Push")
	}
	if err := executor.Respond(types.StepPush, types.ActionSkip, nil); err == nil {
		t.Fatal("generic AXI skip resolved publication Push")
	}
	close(adapter.pushGO) // fake for the separately persisted publication authorization

	select {
	case <-adapter.prReached:
	case <-time.After(5 * time.Second):
		t.Fatal("publication never reached pre-PR authorization park")
	}
	_, _, _, pushCalls, prCalls, _, _ := adapter.snapshot()
	if pushCalls != 1 || prCalls != 0 {
		t.Fatalf("effect calls at PR park push=%d pr=%d, want 1/0", pushCalls, prCalls)
	}
	if err := executor.RespondWithOverrides(types.StepPR, types.ActionFix, []string{"f1"}, nil, nil); err == nil {
		t.Fatal("generic AXI fix authorized publication PR")
	}
	if err := executor.Respond(types.StepPR, types.ActionAbort, nil); err == nil {
		t.Fatal("generic AXI abort resolved publication PR")
	}
	close(adapter.prGO) // fake for the separately persisted publication authorization

	waitExecutorDone(t, done)
	calls, _, _, pushCalls, prCalls, ciHead, fixingSeen := adapter.snapshot()
	if !reflect.DeepEqual(calls, types.AllSteps()) {
		t.Fatalf("publication execution order = %v, want %v", calls, types.AllSteps())
	}
	if pushCalls != 1 || prCalls != 1 {
		t.Fatalf("authorized effect calls push=%d pr=%d, want exactly 1 each", pushCalls, prCalls)
	}
	if ciHead != fixture.publication.HeadSHA || ciHead != publicationExecutorHead {
		t.Fatalf("CI observed %q, want exact publication H %q", ciHead, fixture.publication.HeadSHA)
	}
	if fixingSeen {
		t.Fatal("external authorization resumed through the generic fix loop")
	}
}

func TestExecutorFactoryPublicationTerminalizationNeverReconcilesMutableWorkingCheckout(t *testing.T) {
	tests := []struct {
		name       string
		unrelated  bool
		stepFailed bool
	}{
		{name: "success with descendant checkout"},
		{name: "failure with descendant checkout", stepFailed: true},
		{name: "success with unrelated checkout", unrelated: true},
		{name: "failure with unrelated checkout", unrelated: true, stepFailed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newGitBoundPublicationExecutorFixture(t)
			boundHead := fixture.publication.HeadSHA
			if test.unrelated {
				execGit(t, fixture.workDir, "checkout", "--orphan", "unrelated")
				execGit(t, fixture.workDir, "rm", "-rf", ".")
			}
			writeTestFile(t, fixture.workDir, "moved.txt", test.name+"\n")
			execGit(t, fixture.workDir, "add", ".")
			execGit(t, fixture.workDir, "commit", "-m", "advance mutable registered checkout")
			mutableHead, err := git.HeadSHA(context.Background(), fixture.workDir)
			if err != nil {
				t.Fatal(err)
			}
			if mutableHead == boundHead {
				t.Fatal("test did not move the registered checkout away from publication H")
			}

			steps, _ := publicationExecutorSteps()
			adapter := newPublicationStepAdapterFake(false)
			if test.stepFailed {
				adapter.outcomes[types.StepIntent] = &StepOutcome{ExitCode: 1}
			}
			executor := NewExecutor(fixture.database, fixture.paths, nil, nil, steps, nil)
			executor.SetPublicationStepAdapter(adapter)
			executeErr := executor.Execute(context.Background(), fixture.run, fixture.repo, fixture.workDir)
			if test.stepFailed && executeErr == nil {
				t.Fatal("failing publication step unexpectedly completed")
			}
			if !test.stepFailed && executeErr != nil {
				t.Fatalf("complete publication: %v", executeErr)
			}

			gotRun, err := fixture.database.GetRun(fixture.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			gotPublication, err := fixture.database.GetPublication(fixture.publication.PublicationID)
			if err != nil {
				t.Fatal(err)
			}
			if gotRun.HeadSHA != boundHead || fixture.run.HeadSHA != boundHead || gotPublication.HeadSHA != boundHead {
				t.Fatalf("terminal publication lost exact H binding: durable run=%s memory=%s publication=%s want=%s", gotRun.HeadSHA, fixture.run.HeadSHA, gotPublication.HeadSHA, boundHead)
			}
			if gotRun.TerminalHeadVerifiedAt != nil {
				t.Fatalf("publication recorded ordinary mutable-checkout verification: %v", *gotRun.TerminalHeadVerifiedAt)
			}
			if _, err := git.Run(context.Background(), fixture.workDir, "rev-parse", "--verify", custody.RecoveryRef(fixture.run.ID)); err == nil {
				t.Fatal("publication terminalization created an ordinary recovery ref")
			}

			manager, err := publication.NewManager(publication.ManagerDeps{
				DB: fixture.database, Candidate: publicationProjectionCandidate{},
				Push: publicationProjectionPush{}, PR: publicationProjectionPR{}, CI: publicationProjectionCI{},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := manager.Status(context.Background(), fixture.publication.PublicationID)
			if err != nil {
				t.Fatal(err)
			}
			if result.PublicationID != fixture.publication.PublicationID || result.RunID != fixture.run.ID || result.HeadSHA != boundHead {
				t.Fatalf("public result is inconsistent with exact binding: %#v", result)
			}
		})
	}
}

func TestExecutorOrdinaryRunStillReconcilesAndPreservesAdvancedTerminalHead(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	initialHead, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepoWithID("ordinary123", workDir, "https://github.com/example/project.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/ordinary", initialHead, initialHead)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, workDir, "advanced.txt", "ordinary terminal change\n")
	execGit(t, workDir, "add", ".")
	execGit(t, workDir, "commit", "-m", "ordinary pipeline advance")
	advancedHead, err := git.HeadSHA(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepIntent)}, nil)
	if err := executor.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadSHA != advancedHead || got.TerminalHeadVerifiedAt == nil {
		t.Fatalf("ordinary terminal reconciliation changed: run=%#v want head=%s with verification", got, advancedHead)
	}
	preserved, err := git.Run(context.Background(), workDir, "rev-parse", "--verify", custody.RecoveryRef(run.ID)+"^{commit}")
	if err != nil {
		t.Fatalf("ordinary unpublished terminal head was not preserved: %v", err)
	}
	if preserved != advancedHead {
		t.Fatalf("ordinary recovery ref=%s want=%s", preserved, advancedHead)
	}
}
