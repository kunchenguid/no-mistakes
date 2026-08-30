package publication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	testCommitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testCommitB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testCommitC = "cccccccccccccccccccccccccccccccccccccccc"
	testTreeA   = "1111111111111111111111111111111111111111"
	testTreeB   = "2222222222222222222222222222222222222222"
)

var errSimulatedEffectCrash = errors.New("simulated process loss after effect")

type candidateCall struct {
	PublicationID string
	Step          types.StepName
}

type fakeCandidatePort struct {
	snapshots []CandidateSnapshot
	calls     []candidateCall
}

func (f *fakeCandidatePort) Inspect(_ context.Context, publicationID string, step types.StepName) (CandidateSnapshot, error) {
	f.calls = append(f.calls, candidateCall{PublicationID: publicationID, Step: step})
	if len(f.snapshots) == 0 {
		return cleanCandidateSnapshot(), nil
	}
	snapshot := f.snapshots[0]
	f.snapshots = f.snapshots[1:]
	return snapshot, nil
}

type publicationFixture struct {
	db        *db.DB
	repo      *db.Repo
	parsed    ParsedRequest
	candidate *fakeCandidatePort
	push      *fakePushPort
	pr        *fakePRPort
	ci        *fakeCIPort
	manager   *Manager
}

func newPublicationFixture(t *testing.T, suffix string) *publicationFixture {
	t.Helper()

	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	repo, err := database.InsertRepo(
		filepath.Join(t.TempDir(), "candidate"),
		"https://github.com/example/project.git",
		"main",
	)
	if err != nil {
		t.Fatalf("insert repository: %v", err)
	}

	parsed := mustParsedPublicationRequest(t, repo.ID, suffix)
	candidate := &fakeCandidatePort{}
	push := &fakePushPort{}
	pr := &fakePRPort{}
	ci := &fakeCIPort{}
	manager, err := NewManager(ManagerDeps{
		DB:        database,
		Candidate: candidate,
		Push:      push,
		PR:        pr,
		CI:        ci,
	})
	if err != nil {
		t.Fatalf("new publication manager: %v", err)
	}

	return &publicationFixture{
		db:        database,
		repo:      repo,
		parsed:    parsed,
		candidate: candidate,
		push:      push,
		pr:        pr,
		ci:        ci,
		manager:   manager,
	}
}

func (f *publicationFixture) restartManager(t *testing.T) *Manager {
	t.Helper()
	manager, err := NewManager(ManagerDeps{
		DB:        f.db,
		Candidate: f.candidate,
		Push:      f.push,
		PR:        f.pr,
		CI:        f.ci,
	})
	if err != nil {
		t.Fatalf("restart publication manager: %v", err)
	}
	return manager
}

func mustParsedPublicationRequest(t *testing.T, repoID, suffix string) ParsedRequest {
	t.Helper()
	refSuffix := strings.NewReplacer("_", "-", " ", "-").Replace(suffix)
	sha := func(value string) string {
		sum := sha256.Sum256([]byte(value))
		return hex.EncodeToString(sum[:])
	}
	request := Request{
		Protocol: ProtocolV1,
		Factory: FactoryBinding{
			RunID:                "factory-run-" + suffix,
			TerminalT10Sequence:  10,
			RunStatePrefixSHA256: sha("run-state-" + suffix),
			PlanBindingSHA256:    sha("plan-" + suffix),
		},
		WorkContract: WorkContractBinding{
			Path:   "WORK-CONTRACT.toml",
			SHA256: sha("contract-" + suffix),
		},
		BuildIntent: BuildIntentProjection{
			Summary:            "publish the exact protected candidate " + suffix,
			AcceptanceCriteria: []string{"the exact candidate is published", "CI is green at the exact head"},
		},
		Candidate: CandidateBinding{
			RepositoryID: repoID,
			HeadRef:      "refs/heads/feature-" + refSuffix,
			BaseRef:      "refs/heads/main",
			BaseSHA:      testCommitC,
			CommitSHA:    testCommitA,
			TreeSHA:      testTreeA,
		},
		Publisher: PublisherBinding{
			ExecutablePath:   "/opt/pinned/no-mistakes",
			ExecutableSHA256: sha("publisher-" + suffix),
			BuildSHA:         testCommitB,
			Protocol:         ProtocolV1,
		},
		Scopes: PublicationScopes{
			Push: PushScope{
				Mode:           PushModeExactCommit,
				RemoteIdentity: "github.com/example/project",
				DestinationRef: "refs/heads/feature-" + refSuffix,
			},
			PR: PRScope{
				Mode:    PRModeCreateOrUpdateExactHead,
				BaseRef: "refs/heads/main",
				HeadRef: "refs/heads/feature-" + refSuffix,
			},
			CI: CIScope{Mode: CIModeObserveExactHead},
		},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal publication request: %v", err)
	}
	parsed, err := ParseRequest(raw)
	if err != nil {
		t.Fatalf("parse publication request: %v", err)
	}
	return parsed
}

func cleanCandidateSnapshot() CandidateSnapshot {
	return CandidateSnapshot{
		CommitSHA:         testCommitA,
		TreeSHA:           testTreeA,
		TrackedClean:      true,
		IndexClean:        true,
		UntrackedClean:    true,
		RefsSHA256:        strings.Repeat("3", 64),
		ConfigSHA256:      strings.Repeat("4", 64),
		ReplaceRefsSHA256: strings.Repeat("5", 64),
	}
}

func startPublication(t *testing.T, fixture *publicationFixture) Result {
	t.Helper()
	snapshot, err := fixture.manager.Start(context.Background(), fixture.parsed)
	if err != nil {
		t.Fatalf("start publication: %v", err)
	}
	if snapshot.PublicationID != fixture.parsed.PublicationID {
		t.Fatalf("publication id = %q, want %q", snapshot.PublicationID, fixture.parsed.PublicationID)
	}
	return snapshot
}

func TestPublicationStartRejectsForkRoutingBeforeAdmission(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepoWithIDAndFork(
		"abc123def456",
		filepath.Join(t.TempDir(), "candidate"),
		"https://github.com/example/project.git",
		"git@github.com:contributor/project.git",
		"main",
	)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(ManagerDeps{
		DB: database, Candidate: &fakeCandidatePort{}, Push: &fakePushPort{}, PR: &fakePRPort{}, CI: &fakeCIPort{},
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed := mustParsedPublicationRequest(t, repo.ID, "fork-refusal")
	if _, err := manager.Start(context.Background(), parsed); err == nil {
		t.Fatal("factory publication v1 accepted an unresolved fork routing contract")
	}
	publication, err := database.GetPublication(parsed.PublicationID)
	if err != nil {
		t.Fatal(err)
	}
	if publication != nil {
		t.Fatalf("fork refusal happened after durable admission: %#v", publication)
	}
}

func completeDefenseThroughLint(t *testing.T, fixture *publicationFixture) Result {
	t.Helper()
	publicationID := fixture.parsed.PublicationID
	for _, step := range []types.StepName{
		types.StepIntent,
		types.StepRebase,
		types.StepReview,
		types.StepTest,
		types.StepDocument,
		types.StepLint,
	} {
		if err := fixture.manager.BeginStep(context.Background(), publicationID, step); err != nil {
			t.Fatalf("begin %s: %v", step, err)
		}
		snapshot, err := fixture.manager.CompleteStep(context.Background(), publicationID, step, StepOutcomePass)
		if err != nil {
			t.Fatalf("complete %s: %v", step, err)
		}
		if step == types.StepLint {
			return snapshot
		}
	}
	t.Fatal("lint was not executed")
	return Result{}
}

func TestPublicationManagerIsOnlyAGuardAndEffectServiceForTheExistingExecutor(t *testing.T) {
	want := types.AllSteps()
	got := PublicationStepPlan()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("publication steps = %v, want existing executor order %v", got, want)
	}

	managerType := reflect.TypeOf(&Manager{})
	for _, forbidden := range []string{"Drive", "Next", "Run", "Retry", "ResumeLoop"} {
		if _, exists := managerType.MethodByName(forbidden); exists {
			t.Fatalf("Manager.%s makes publication a second executor/loop", forbidden)
		}
	}
}

func TestPublicationStartNeverAttachesAnOrdinaryAXIRun(t *testing.T) {
	fixture := newPublicationFixture(t, "no-attach")
	ordinary, err := fixture.db.InsertRun(
		fixture.repo.ID,
		strings.TrimPrefix(fixture.parsed.Request.Candidate.HeadRef, "refs/heads/"),
		fixture.parsed.Request.Candidate.CommitSHA,
		fixture.parsed.Request.Candidate.CommitSHA,
	)
	if err != nil {
		t.Fatalf("insert ordinary AXI run: %v", err)
	}

	if _, err := fixture.manager.Start(context.Background(), fixture.parsed); !errors.Is(err, db.ErrPublicationRunConflict) {
		t.Fatalf("publication beside an active ordinary AXI run error = %v, want ErrPublicationRunConflict", err)
	}
	ordinaryAgain, err := fixture.db.GetRun(ordinary.ID)
	if err != nil {
		t.Fatalf("get ordinary run: %v", err)
	}
	if ordinaryAgain.Kind != db.RunKindStandard {
		t.Fatalf("ordinary run kind changed to %q", ordinaryAgain.Kind)
	}
	if publication, err := fixture.db.GetPublication(fixture.parsed.PublicationID); err != nil {
		t.Fatalf("get refused publication: %v", err)
	} else if publication != nil {
		t.Fatalf("ordinary-run conflict persisted publication %#v", publication)
	}
}

func TestPublicationStartPersistsExactBaseSHAInPublicationAndRun(t *testing.T) {
	fixture := newPublicationFixture(t, "exact-base")
	startPublication(t, fixture)

	publication, err := fixture.db.GetPublication(fixture.parsed.PublicationID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := fixture.db.GetRun(publication.RunID)
	if err != nil {
		t.Fatal(err)
	}
	want := fixture.parsed.Request.Candidate.BaseSHA
	if publication.BaseSHA != want || run.BaseSHA != want {
		t.Fatalf("base binding publication=%q run=%q, want exact %q", publication.BaseSHA, run.BaseSHA, want)
	}
	if err := fixture.manager.ValidateIntent(context.Background(), fixture.parsed.PublicationID); err != nil {
		t.Fatalf("exact base binding failed intent reproof: %v", err)
	}
}

func TestPublicationDefenseStepsGuardExactReadOnlyCandidateBeforeAndAfter(t *testing.T) {
	fixture := newPublicationFixture(t, "defense-read-only")
	startPublication(t, fixture)

	snapshot := completeDefenseThroughLint(t, fixture)
	if snapshot.Status != StatusReadyForPush {
		t.Fatalf("status after lint = %q, want %q", snapshot.Status, StatusReadyForPush)
	}

	wantSteps := []types.StepName{
		types.StepRebase, types.StepRebase,
		types.StepReview, types.StepReview,
		types.StepTest, types.StepTest,
		types.StepDocument, types.StepDocument,
		types.StepLint, types.StepLint,
	}
	var gotSteps []types.StepName
	for _, call := range fixture.candidate.calls {
		gotSteps = append(gotSteps, call.Step)
	}
	if !reflect.DeepEqual(gotSteps, wantSteps) {
		t.Fatalf("candidate guards = %v, want before/after for each defense step %v", gotSteps, wantSteps)
	}
}

func TestPublicationDefenseMutationFailsClosedWithoutRetry(t *testing.T) {
	cases := map[string]func(*CandidateSnapshot){
		"head":         func(s *CandidateSnapshot) { s.CommitSHA = testCommitB },
		"tree":         func(s *CandidateSnapshot) { s.TreeSHA = testTreeB },
		"tracked":      func(s *CandidateSnapshot) { s.TrackedClean = false },
		"index":        func(s *CandidateSnapshot) { s.IndexClean = false },
		"untracked":    func(s *CandidateSnapshot) { s.UntrackedClean = false },
		"refs":         func(s *CandidateSnapshot) { s.RefsSHA256 = strings.Repeat("6", 64) },
		"config":       func(s *CandidateSnapshot) { s.ConfigSHA256 = strings.Repeat("7", 64) },
		"replace_refs": func(s *CandidateSnapshot) { s.ReplaceRefsSHA256 = strings.Repeat("8", 64) },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newPublicationFixture(t, "drift-"+name)
			startPublication(t, fixture)
			before := cleanCandidateSnapshot()
			after := before
			mutate(&after)
			fixture.candidate.snapshots = []CandidateSnapshot{before, after}

			if err := fixture.manager.BeginStep(context.Background(), fixture.parsed.PublicationID, types.StepReview); err != nil {
				t.Fatalf("begin review: %v", err)
			}
			snapshot, err := fixture.manager.CompleteStep(context.Background(), fixture.parsed.PublicationID, types.StepReview, StepOutcomePass)
			if err != nil {
				t.Fatalf("complete drifted review: %v", err)
			}
			if snapshot.Status != StatusDrift {
				t.Fatalf("status = %q, want %q", snapshot.Status, StatusDrift)
			}
			if err := fixture.manager.BeginStep(context.Background(), fixture.parsed.PublicationID, types.StepReview); err == nil {
				t.Fatal("drifted defense step was allowed to retry")
			}
			if len(fixture.candidate.calls) != 2 {
				t.Fatalf("candidate inspected %d times, want exactly before+after without retry", len(fixture.candidate.calls))
			}
		})
	}
}

func TestPublicationDefenseNeverConvertsNonPassIntoFixRetryOrSkip(t *testing.T) {
	for _, outcome := range []StepOutcome{
		StepOutcomeFail,
		StepOutcomeError,
		StepOutcomeSkipped,
		StepOutcomePartial,
		StepOutcomeNotExecuted,
		StepOutcome("unknown"),
		StepOutcome("malformed-value"),
	} {
		t.Run(string(outcome), func(t *testing.T) {
			fixture := newPublicationFixture(t, "outcome-"+string(outcome))
			startPublication(t, fixture)
			if err := fixture.manager.BeginStep(context.Background(), fixture.parsed.PublicationID, types.StepReview); err != nil {
				t.Fatalf("begin review: %v", err)
			}
			snapshot, err := fixture.manager.CompleteStep(context.Background(), fixture.parsed.PublicationID, types.StepReview, outcome)
			if err != nil {
				t.Fatalf("record fail-closed outcome: %v", err)
			}
			if snapshot.Status == StatusReady || snapshot.Status == StatusReadyForPush || snapshot.Status == StatusChecking {
				t.Fatalf("outcome %q produced success/nonterminal quality status %q", outcome, snapshot.Status)
			}
			if err := fixture.manager.BeginStep(context.Background(), fixture.parsed.PublicationID, types.StepReview); err == nil {
				t.Fatalf("outcome %q was treated as retryable", outcome)
			}
		})
	}
}
