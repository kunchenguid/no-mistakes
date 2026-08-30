package steps

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/publication"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	adapterPublicationID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	adapterHeadSHA       = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type adapterTrace struct {
	mu     sync.Mutex
	events []string
}

func (t *adapterTrace) add(event string) {
	t.mu.Lock()
	t.events = append(t.events, event)
	t.mu.Unlock()
}

func (t *adapterTrace) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.events...)
}

type fakePublicationCandidatePort struct {
	trace *adapterTrace
	view  publication.CandidateStepView
}

func (f *fakePublicationCandidatePort) PrepareStep(_ context.Context, publicationID string, step types.StepName) (publication.CandidateStepView, error) {
	f.trace.add("prepare:" + string(step))
	if publicationID != adapterPublicationID {
		return publication.CandidateStepView{}, errors.New("wrong publication id")
	}
	return f.view, nil
}

func (f *fakePublicationCandidatePort) DisposeStep(_ context.Context, publicationID string, step types.StepName) error {
	f.trace.add("dispose:" + string(step))
	if publicationID != adapterPublicationID {
		return errors.New("wrong publication id")
	}
	return nil
}

type fakePublicationFreshnessPort struct {
	trace *adapterTrace
	calls int
	view  publication.CandidateStepView
}

func (f *fakePublicationFreshnessPort) CheckUpToDate(_ context.Context, publicationID string, view publication.CandidateStepView) error {
	f.calls++
	f.view = view
	f.trace.add("freshness:rebase")
	if publicationID != adapterPublicationID {
		return errors.New("wrong publication id")
	}
	return nil
}

type fakePublicationExecutionManager struct {
	trace *adapterTrace

	intentCalls int
	before      []types.StepName
	after       []types.StepName
	afterResult []publication.StepOutcome

	beginStepCalls    int
	completeStepCalls int

	pushPrepared int
	pushExecuted int
	pushReached  chan struct{}
	pushGO       chan struct{}

	prPrepared int
	prExecuted int
	prDraft    []byte
	prReached  chan struct{}
	prGO       chan struct{}

	ciObserved int
	ciResults  []publication.Result
}

func newFakePublicationExecutionManager(trace *adapterTrace) *fakePublicationExecutionManager {
	return &fakePublicationExecutionManager{
		trace:       trace,
		pushReached: make(chan struct{}),
		pushGO:      make(chan struct{}),
		prReached:   make(chan struct{}),
		prGO:        make(chan struct{}),
	}
}

func (f *fakePublicationExecutionManager) ValidateIntent(_ context.Context, publicationID string) error {
	f.intentCalls++
	f.trace.add("validate:intent")
	if publicationID != adapterPublicationID {
		return errors.New("wrong publication id")
	}
	return nil
}

func (f *fakePublicationExecutionManager) BeforeDefense(_ context.Context, publicationID string, step types.StepName) error {
	f.before = append(f.before, step)
	f.trace.add("guard-before:" + string(step))
	if publicationID != adapterPublicationID {
		return errors.New("wrong publication id")
	}
	return nil
}

func (f *fakePublicationExecutionManager) AfterDefense(_ context.Context, publicationID string, step types.StepName, outcome publication.StepOutcome) error {
	f.after = append(f.after, step)
	f.afterResult = append(f.afterResult, outcome)
	f.trace.add("guard-after:" + string(step))
	if publicationID != adapterPublicationID {
		return errors.New("wrong publication id")
	}
	return nil
}

func (f *fakePublicationExecutionManager) PreparePush(_ context.Context, publicationID string) (publication.EffectChallenge, error) {
	f.pushPrepared++
	f.trace.add("prepare:push-effect")
	return publication.EffectChallenge{
		PublicationID: publicationID,
		Kind:          publication.EffectPush,
		CommitSHA:     adapterHeadSHA,
	}, nil
}

func (f *fakePublicationExecutionManager) PreparePR(_ context.Context, publicationID string, draft []byte) (publication.EffectChallenge, error) {
	f.prPrepared++
	f.prDraft = append([]byte(nil), draft...)
	f.trace.add("prepare:pr-effect")
	return publication.EffectChallenge{
		PublicationID: publicationID,
		Kind:          publication.EffectPR,
		CommitSHA:     adapterHeadSHA,
	}, nil
}

func (f *fakePublicationExecutionManager) WaitForAuthorization(ctx context.Context, challenge publication.EffectChallenge) error {
	switch challenge.Kind {
	case publication.EffectPush:
		f.trace.add("park:push")
		close(f.pushReached)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.pushGO:
			f.trace.add("authorized:push")
			return nil
		}
	case publication.EffectPR:
		f.trace.add("park:pr")
		close(f.prReached)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.prGO:
			f.trace.add("authorized:pr")
			return nil
		}
	default:
		return errors.New("unexpected effect kind")
	}
}

func (f *fakePublicationExecutionManager) ExecutePush(_ context.Context, publicationID string) (publication.Result, error) {
	f.pushExecuted++
	f.trace.add("execute:push")
	return publication.Result{
		Protocol:      publication.ProtocolV1,
		PublicationID: publicationID,
		RunID:         "factory-run",
		HeadSHA:       adapterHeadSHA,
		Status:        publication.StatusReadyForPR,
	}, nil
}

func (f *fakePublicationExecutionManager) ExecutePR(_ context.Context, publicationID string) (publication.Result, error) {
	f.prExecuted++
	f.trace.add("execute:pr")
	return publication.Result{
		Protocol:      publication.ProtocolV1,
		PublicationID: publicationID,
		RunID:         "factory-run",
		HeadSHA:       adapterHeadSHA,
		Status:        publication.StatusCIObserving,
	}, nil
}

func (f *fakePublicationExecutionManager) ObserveCI(_ context.Context, publicationID string) (publication.Result, error) {
	f.ciObserved++
	f.trace.add("observe:ci")
	if len(f.ciResults) > 0 {
		result := f.ciResults[0]
		f.ciResults = f.ciResults[1:]
		return result, nil
	}
	return publication.Result{
		Protocol:      publication.ProtocolV1,
		PublicationID: publicationID,
		RunID:         "factory-run",
		HeadSHA:       adapterHeadSHA,
		Status:        publication.StatusReady,
	}, nil
}

// These legacy methods intentionally exist on the fake so the test can prove
// the adapter never reaches for Manager's step-lifecycle writers. Executor is
// the only owner of step_results status transitions.
func (f *fakePublicationExecutionManager) BeginStep(context.Context, string, types.StepName) error {
	f.beginStepCalls++
	return errors.New("adapter called forbidden Manager.BeginStep")
}

func (f *fakePublicationExecutionManager) CompleteStep(context.Context, string, types.StepName, publication.StepOutcome) (publication.Result, error) {
	f.completeStepCalls++
	return publication.Result{}, errors.New("adapter called forbidden Manager.CompleteStep")
}

type adapterStep struct {
	name    types.StepName
	trace   *adapterTrace
	calls   int
	seen    *pipeline.StepContext
	outcome *pipeline.StepOutcome
	err     error
}

func (s *adapterStep) Name() types.StepName { return s.name }

func (s *adapterStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.calls++
	copy := *sctx
	s.seen = &copy
	s.trace.add("execute-existing:" + string(s.name))
	if s.err != nil {
		return nil, s.err
	}
	if s.outcome != nil {
		return s.outcome, nil
	}
	return &pipeline.StepOutcome{}, nil
}

func newAdapterFixture(t *testing.T) (pipeline.PublicationStepAdapter, *fakePublicationExecutionManager, *fakePublicationCandidatePort, *fakePublicationFreshnessPort, *adapterTrace, *pipeline.StepContext) {
	t.Helper()
	trace := &adapterTrace{}
	root := t.TempDir()
	view := publication.CandidateStepView{
		WorktreeDir:     filepath.Join(root, "candidate", "view"),
		ScratchDir:      filepath.Join(root, "scratch"),
		WorkContractRaw: []byte("version = 1\n"),
	}
	candidate := &fakePublicationCandidatePort{trace: trace, view: view}
	freshness := &fakePublicationFreshnessPort{trace: trace}
	manager := newFakePublicationExecutionManager(trace)
	adapter, err := NewFactoryPublicationStepAdapter(FactoryPublicationStepAdapterOptions{
		PublicationID: adapterPublicationID,
		Manager:       manager,
		Candidate:     candidate,
		Freshness:     freshness,
		RenderPRDraft: func(_ context.Context, publicationID string) ([]byte, error) {
			if publicationID != adapterPublicationID {
				return nil, errors.New("wrong publication id")
			}
			trace.add("render:pr-draft")
			return []byte("exact rendered draft\n"), nil
		},
	})
	if err != nil {
		t.Fatalf("new publication step adapter: %v", err)
	}
	sctx := &pipeline.StepContext{
		Ctx:         context.Background(),
		Run:         &db.Run{ID: "run-1", Kind: types.RunKindFactoryPublicationV1, HeadSHA: adapterHeadSHA},
		Repo:        &db.Repo{ID: "repo-1"},
		WorkDir:     filepath.Join(root, "ordinary-worktree"),
		EvidenceDir: filepath.Join(root, "ordinary-evidence"),
		Env:         []string{"KEEP=value"},
	}
	return adapter, manager, candidate, freshness, trace, sctx
}

func TestFactoryPublicationStepAdapterValidatesIntentAndUsesReadOnlyRebaseCheck(t *testing.T) {
	adapter, manager, _, freshness, trace, sctx := newAdapterFixture(t)

	intent := &adapterStep{name: types.StepIntent, trace: trace}
	outcome, err := adapter.ExecutePublicationStep(sctx, intent)
	if err != nil {
		t.Fatalf("execute Intent adapter: %v", err)
	}
	if outcome == nil || outcome.NeedsApproval || manager.intentCalls != 1 {
		t.Fatalf("Intent outcome=%#v validation calls=%d, want one closed validation", outcome, manager.intentCalls)
	}
	if intent.calls != 0 {
		t.Fatalf("ordinary Intent executed %d times; adapter must validate the bound request", intent.calls)
	}

	rebase := &adapterStep{name: types.StepRebase, trace: trace}
	outcome, err = adapter.ExecutePublicationStep(sctx, rebase)
	if err != nil {
		t.Fatalf("execute Rebase adapter: %v", err)
	}
	if outcome == nil || outcome.NeedsApproval || freshness.calls != 1 {
		t.Fatalf("Rebase outcome=%#v freshness calls=%d, want one read-only check", outcome, freshness.calls)
	}
	if rebase.calls != 0 {
		t.Fatalf("ordinary mutating Rebase executed %d times", rebase.calls)
	}
	want := []string{
		"validate:intent",
		"prepare:rebase",
		"guard-before:rebase",
		"freshness:rebase",
		"guard-after:rebase",
		"dispose:rebase",
	}
	if got := trace.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Intent/Rebase composition = %v, want %v", got, want)
	}
	if manager.beginStepCalls != 0 || manager.completeStepCalls != 0 {
		t.Fatalf("adapter used Manager step lifecycle writers: begin=%d complete=%d", manager.beginStepCalls, manager.completeStepCalls)
	}
}

func TestFactoryPublicationStepAdapterRunsExistingDefenseInFreshGuardedView(t *testing.T) {
	for _, name := range []types.StepName{
		types.StepReview,
		types.StepTest,
		types.StepDocument,
		types.StepLint,
	} {
		t.Run(string(name), func(t *testing.T) {
			adapter, manager, candidate, _, trace, sctx := newAdapterFixture(t)
			originalWorkDir := sctx.WorkDir
			originalEvidenceDir := sctx.EvidenceDir
			step := &adapterStep{name: name, trace: trace}

			outcome, err := adapter.ExecutePublicationStep(sctx, step)
			if err != nil {
				t.Fatalf("execute %s adapter: %v", name, err)
			}
			if outcome == nil || outcome.NeedsApproval || outcome.AutoFixable || outcome.Skipped || outcome.RestartFrom != "" {
				t.Fatalf("%s returned open/generic outcome %#v", name, outcome)
			}
			if step.calls != 1 || step.seen == nil {
				t.Fatalf("existing %s step calls=%d, want exactly one", name, step.calls)
			}
			if step.seen.WorkDir != candidate.view.WorktreeDir {
				t.Fatalf("%s workdir=%q, want prepared view %q", name, step.seen.WorkDir, candidate.view.WorktreeDir)
			}
			if !step.seen.PublicationDefense {
				t.Fatalf("%s candidate context did not enable publication defense", name)
			}
			if step.seen.EvidenceDir == "" || pathIsWithin(step.seen.EvidenceDir, candidate.view.WorktreeDir) || !pathIsWithin(step.seen.EvidenceDir, candidate.view.ScratchDir) {
				t.Fatalf("%s evidence/scratch dir %q is not outside candidate and under scratch %q", name, step.seen.EvidenceDir, candidate.view.ScratchDir)
			}
			if sctx.WorkDir != originalWorkDir || sctx.EvidenceDir != originalEvidenceDir {
				t.Fatalf("adapter mutated caller context: workdir=%q evidence=%q", sctx.WorkDir, sctx.EvidenceDir)
			}
			if sctx.PublicationDefense {
				t.Fatal("adapter leaked publication defense into caller context")
			}
			want := []string{
				"prepare:" + string(name),
				"guard-before:" + string(name),
				"execute-existing:" + string(name),
				"guard-after:" + string(name),
				"dispose:" + string(name),
			}
			if got := trace.snapshot(); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s composition = %v, want %v", name, got, want)
			}
			if !reflect.DeepEqual(manager.before, []types.StepName{name}) || !reflect.DeepEqual(manager.after, []types.StepName{name}) || !reflect.DeepEqual(manager.afterResult, []publication.StepOutcome{publication.StepOutcomePass}) {
				t.Fatalf("%s guards before=%v after=%v outcomes=%v", name, manager.before, manager.after, manager.afterResult)
			}
			if manager.beginStepCalls != 0 || manager.completeStepCalls != 0 {
				t.Fatalf("adapter used Manager step lifecycle writers: begin=%d complete=%d", manager.beginStepCalls, manager.completeStepCalls)
			}
		})
	}
}

func TestFactoryPublicationStepAdapterAcceptsOnlyClosedNonBlockingFindingPayloads(t *testing.T) {
	tests := map[string]struct {
		outcome *pipeline.StepOutcome
		wantErr bool
	}{
		"real empty success": {
			outcome: &pipeline.StepOutcome{
				Findings:   `{"findings":[],"summary":"all checks passed","tested":["go test ./..."],"testing_summary":"pass"}`,
				FixSummary: "verified requested behavior",
			},
		},
		"informational no-op": {
			outcome: &pipeline.StepOutcome{Findings: `{"findings":[{"severity":"info","description":"evidence captured","action":"no-op"}],"summary":"evidence only"}`},
		},
		"malformed findings": {
			outcome: &pipeline.StepOutcome{Findings: `{"findings":`},
			wantErr: true,
		},
		"missing findings array": {
			outcome: &pipeline.StepOutcome{Findings: `{"summary":"not a findings payload"}`},
			wantErr: true,
		},
		"actionable finding": {
			outcome: &pipeline.StepOutcome{Findings: `{"findings":[{"severity":"warning","description":"must change","action":"auto-fix"}],"summary":"blocked"}`},
			wantErr: true,
		},
		"generic fix request": {
			outcome: &pipeline.StepOutcome{Findings: `{"findings":[],"summary":"clean"}`, AutoFixable: true},
			wantErr: true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			adapter, _, _, _, _, sctx := newAdapterFixture(t)
			step := &adapterStep{name: types.StepTest, trace: &adapterTrace{}, outcome: test.outcome}
			outcome, err := adapter.ExecutePublicationStep(sctx, step)
			if test.wantErr {
				if err == nil {
					t.Fatalf("outcome %#v passed protected defense", test.outcome)
				}
				return
			}
			if err != nil {
				t.Fatalf("closed success outcome rejected: %v", err)
			}
			if outcome == nil || outcome.Findings != "" || outcome.FixSummary != "" || outcome.NeedsApproval || outcome.AutoFixable {
				t.Fatalf("adapter did not project a closed pass: %#v", outcome)
			}
		})
	}
}

func TestFactoryPublicationStepAdapterDisposesViewAndGuardsAfterDefenseError(t *testing.T) {
	adapter, manager, _, _, trace, sctx := newAdapterFixture(t)
	step := &adapterStep{name: types.StepReview, trace: trace, err: errors.New("defense failed")}

	if _, err := adapter.ExecutePublicationStep(sctx, step); err == nil {
		t.Fatal("failed defense returned success")
	}
	want := []string{
		"prepare:review",
		"guard-before:review",
		"execute-existing:review",
		"guard-after:review",
		"dispose:review",
	}
	if got := trace.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("failed defense cleanup = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(manager.afterResult, []publication.StepOutcome{publication.StepOutcomeError}) {
		t.Fatalf("failed defense guard outcome = %v, want ERROR", manager.afterResult)
	}
}

func TestFactoryPublicationStepAdapterRetainsCandidateWhenBoundaryCleanupIsUncertain(t *testing.T) {
	adapter, manager, _, _, trace, sctx := newAdapterFixture(t)
	step := &adapterStep{
		name:  types.StepReview,
		trace: trace,
		err:   fmt.Errorf("protected launch teardown: %w", agent.ErrPublicationConfinementCleanupUncertain),
	}

	_, err := adapter.ExecutePublicationStep(sctx, step)
	if !errors.Is(err, agent.ErrPublicationConfinementCleanupUncertain) {
		t.Fatalf("uncertain cleanup error=%v, want distinguished cleanup error", err)
	}
	want := []string{
		"prepare:review",
		"guard-before:review",
		"execute-existing:review",
		"guard-after:review",
	}
	if got := trace.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("uncertain cleanup trace=%v, want retained candidate without dispose %v", got, want)
	}
	if !reflect.DeepEqual(manager.afterResult, []publication.StepOutcome{publication.StepOutcomeError}) {
		t.Fatalf("uncertain cleanup guard outcome=%v, want ERROR", manager.afterResult)
	}
}

func TestFactoryPublicationStepAdapterParksPushAndPRUntilExternalAuthorization(t *testing.T) {
	adapter, manager, _, _, trace, sctx := newAdapterFixture(t)

	pushDone := make(chan struct {
		outcome *pipeline.StepOutcome
		err     error
	}, 1)
	go func() {
		outcome, err := adapter.ExecutePublicationStep(sctx, &adapterStep{name: types.StepPush, trace: trace})
		pushDone <- struct {
			outcome *pipeline.StepOutcome
			err     error
		}{outcome: outcome, err: err}
	}()
	select {
	case <-manager.pushReached:
	case <-time.After(5 * time.Second):
		t.Fatal("Push did not park before its external port")
	}
	if manager.pushExecuted != 0 {
		t.Fatalf("Push executed %d times before external authorization", manager.pushExecuted)
	}
	close(manager.pushGO)
	pushResult := <-pushDone
	if pushResult.err != nil || pushResult.outcome == nil || pushResult.outcome.NeedsApproval {
		t.Fatalf("authorized Push outcome=%#v error=%v", pushResult.outcome, pushResult.err)
	}
	if manager.pushPrepared != 1 || manager.pushExecuted != 1 {
		t.Fatalf("Push prepare/execute=%d/%d, want 1/1", manager.pushPrepared, manager.pushExecuted)
	}

	prDone := make(chan struct {
		outcome *pipeline.StepOutcome
		err     error
	}, 1)
	go func() {
		outcome, err := adapter.ExecutePublicationStep(sctx, &adapterStep{name: types.StepPR, trace: trace})
		prDone <- struct {
			outcome *pipeline.StepOutcome
			err     error
		}{outcome: outcome, err: err}
	}()
	select {
	case <-manager.prReached:
	case <-time.After(5 * time.Second):
		t.Fatal("PR did not park before its external port")
	}
	if manager.prExecuted != 0 {
		t.Fatalf("PR executed %d times before external authorization", manager.prExecuted)
	}
	close(manager.prGO)
	prResult := <-prDone
	if prResult.err != nil || prResult.outcome == nil || prResult.outcome.NeedsApproval {
		t.Fatalf("authorized PR outcome=%#v error=%v", prResult.outcome, prResult.err)
	}
	if manager.prPrepared != 1 || manager.prExecuted != 1 || string(manager.prDraft) != "exact rendered draft\n" {
		t.Fatalf("PR prepare/execute=%d/%d draft=%q", manager.prPrepared, manager.prExecuted, manager.prDraft)
	}

	wantEffectOrder := []string{
		"prepare:push-effect", "park:push", "authorized:push", "execute:push",
		"render:pr-draft", "prepare:pr-effect", "park:pr", "authorized:pr", "execute:pr",
	}
	if got := trace.snapshot(); !reflect.DeepEqual(got, wantEffectOrder) {
		t.Fatalf("effect composition = %v, want %v", got, wantEffectOrder)
	}
	if manager.beginStepCalls != 0 || manager.completeStepCalls != 0 {
		t.Fatalf("adapter used Manager step lifecycle writers: begin=%d complete=%d", manager.beginStepCalls, manager.completeStepCalls)
	}
}

func TestFactoryPublicationStepAdapterCIUsesReadOnlyManagerObservation(t *testing.T) {
	adapter, manager, _, _, trace, sctx := newAdapterFixture(t)
	ordinaryCI := &adapterStep{name: types.StepCI, trace: trace}

	outcome, err := adapter.ExecutePublicationStep(sctx, ordinaryCI)
	if err != nil {
		t.Fatalf("observe publication CI: %v", err)
	}
	if outcome == nil || outcome.NeedsApproval || outcome.RestartFrom != "" || outcome.AutoFixable {
		t.Fatalf("CI returned generic retry/approval outcome %#v", outcome)
	}
	if manager.ciObserved != 1 {
		t.Fatalf("CI observations=%d, want 1", manager.ciObserved)
	}
	if ordinaryCI.calls != 0 {
		t.Fatalf("ordinary mutating CI step executed %d times", ordinaryCI.calls)
	}
	if got := trace.snapshot(); !reflect.DeepEqual(got, []string{"observe:ci"}) {
		t.Fatalf("CI composition=%v, want read-only observation only", got)
	}
}

func TestFactoryPublicationStepAdapterWaitsForPendingExactHeadCIWithoutExecutorRetry(t *testing.T) {
	adapter, manager, _, _, trace, sctx := newAdapterFixture(t)
	manager.ciResults = []publication.Result{
		{Protocol: publication.ProtocolV1, PublicationID: adapterPublicationID, RunID: "factory-run", HeadSHA: adapterHeadSHA, Status: publication.StatusCIObserving},
		{Protocol: publication.ProtocolV1, PublicationID: adapterPublicationID, RunID: "factory-run", HeadSHA: adapterHeadSHA, Status: publication.StatusReady},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sctx.Ctx = ctx
	ordinaryCI := &adapterStep{name: types.StepCI, trace: trace}

	outcome, err := adapter.ExecutePublicationStep(sctx, ordinaryCI)
	if err != nil {
		t.Fatalf("wait for exact-H CI: %v", err)
	}
	if outcome == nil || outcome.NeedsApproval || outcome.RestartFrom != "" || outcome.AutoFixable {
		t.Fatalf("CI wait returned generic retry/approval outcome %#v", outcome)
	}
	if manager.ciObserved != 2 {
		t.Fatalf("CI observations=%d, want pending then READY", manager.ciObserved)
	}
	if ordinaryCI.calls != 0 {
		t.Fatalf("ordinary CI step executed %d times", ordinaryCI.calls)
	}
	if got := trace.snapshot(); !reflect.DeepEqual(got, []string{"observe:ci", "observe:ci"}) {
		t.Fatalf("CI technical polling composition=%v", got)
	}
}

func TestFactoryPublicationStepAdapterRequiresEveryService(t *testing.T) {
	validDraft := func(context.Context, string) ([]byte, error) { return []byte("draft"), nil }
	trace := &adapterTrace{}
	manager := newFakePublicationExecutionManager(trace)
	candidate := &fakePublicationCandidatePort{trace: trace}
	freshness := &fakePublicationFreshnessPort{trace: trace}

	cases := map[string]FactoryPublicationStepAdapterOptions{
		"publication id": {Manager: manager, Candidate: candidate, Freshness: freshness, RenderPRDraft: validDraft},
		"manager":        {PublicationID: adapterPublicationID, Candidate: candidate, Freshness: freshness, RenderPRDraft: validDraft},
		"candidate":      {PublicationID: adapterPublicationID, Manager: manager, Freshness: freshness, RenderPRDraft: validDraft},
		"freshness":      {PublicationID: adapterPublicationID, Manager: manager, Candidate: candidate, RenderPRDraft: validDraft},
		"PR draft":       {PublicationID: adapterPublicationID, Manager: manager, Candidate: candidate, Freshness: freshness},
	}
	for name, options := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewFactoryPublicationStepAdapter(options); err == nil {
				t.Fatalf("constructor accepted missing %s service", name)
			}
		})
	}
}

func pathIsWithin(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && len(rel) > 0 && rel[:1] != "."
}
