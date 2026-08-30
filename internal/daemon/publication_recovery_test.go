package daemon

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/publication"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func openPublicationRecoveryDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "no-mistakes.sqlite"))
	if err != nil {
		t.Fatalf("open recovery database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

type publicationRecoveryCall struct {
	operation     string
	publicationID string
	kind          publication.EffectKind
}

// fakePublicationRecoveryService deliberately exposes no ExecutePush or
// ExecutePR method. Startup may resume repeatable read-only work or reconcile
// an effect that may already have happened; it has no capability to replay a
// mutating effect.
type fakePublicationRecoveryService struct {
	calls       []publicationRecoveryCall
	resumeCheck func(string) error
	recover     func(string, publication.EffectKind) (publication.Result, error)
}

func (f *fakePublicationRecoveryService) ResumePublication(_ context.Context, publicationID string) (publication.Result, error) {
	f.calls = append(f.calls, publicationRecoveryCall{operation: "resume", publicationID: publicationID})
	if f.resumeCheck != nil {
		if err := f.resumeCheck(publicationID); err != nil {
			return publication.Result{}, err
		}
	}
	return publication.Result{Protocol: publication.ProtocolV1, PublicationID: publicationID, Status: publication.StatusChecking}, nil
}

func (f *fakePublicationRecoveryService) RecoverEffect(_ context.Context, publicationID string, kind publication.EffectKind) (publication.Result, error) {
	f.calls = append(f.calls, publicationRecoveryCall{operation: "reconcile", publicationID: publicationID, kind: kind})
	if f.recover != nil {
		return f.recover(publicationID, kind)
	}
	return publication.Result{}, fmt.Errorf("unexpected effect reconciliation")
}

func createDaemonRecoveryPublication(t *testing.T, database *db.DB, suffix string) (*db.Publication, *db.Run) {
	t.Helper()
	repoID := "publication-recovery-" + suffix
	if _, err := database.InsertRepoWithID(
		repoID,
		"/work/"+repoID,
		"https://github.com/example/"+repoID+".git",
		"main",
	); err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	request := []byte(fmt.Sprintf(`{"protocol":"factory-publication-v1","case":%q}`, suffix))
	digest := sha256.Sum256(request)
	publicationRow, run, created, err := database.CreateOrGetPublication(db.CreatePublicationInput{
		PublicationID:    fmt.Sprintf("%x", digest),
		CanonicalRequest: request,
		RepoID:           repoID,
		CandidateRef:     "refs/heads/feature/" + suffix,
		BaseRef:          "refs/heads/main",
		BaseSHA:          "0000000000000000000000000000000000000000",
		HeadSHA:          "1111111111111111111111111111111111111111",
		TreeSHA:          "2222222222222222222222222222222222222222",
	})
	if err != nil {
		t.Fatalf("create publication: %v", err)
	}
	if !created {
		t.Fatal("first publication admission reconciled an existing run")
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatalf("mark publication running: %v", err)
	}
	run.Status = types.RunRunning
	return publicationRow, run
}

func publicationStep(t *testing.T, database *db.DB, runID string, name types.StepName) *db.StepResult {
	t.Helper()
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatalf("get publication steps: %v", err)
	}
	for _, step := range steps {
		if step.StepName == name {
			return step
		}
	}
	t.Fatalf("publication step %s is missing", name)
	return nil
}

func recoveryEffectBinding(kind db.PublicationEffectKind) db.PublicationEffectBinding {
	binding := db.PublicationEffectBinding{
		CandidateSHA:   "1111111111111111111111111111111111111111",
		RemoteIdentity: "github.com/example/repository",
		DestinationRef: "refs/heads/feature/recovery",
		HeadRef:        "refs/heads/feature/recovery",
		EffectDigest:   "effect-digest-" + string(kind),
	}
	if kind == db.PublicationEffectPR || kind == db.PublicationEffectCI {
		binding.BaseRef = "refs/heads/main"
	}
	if kind == db.PublicationEffectPR {
		binding.DraftDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}
	return binding
}

func TestPublicationStartupRecoveryIsDiscoveredSeparatelyFromOrdinaryStaleFailure(t *testing.T) {
	database := openPublicationRecoveryDB(t)
	publicationRow, publicationRun := createDaemonRecoveryPublication(t, database, "separate")
	repo, err := database.InsertRepoWithID("ordinary-recovery", "/work/ordinary-recovery", "https://github.com/example/ordinary.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	ordinaryRun, err := database.InsertRun(repo.ID, "feature/ordinary", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(ordinaryRun.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}

	service := &fakePublicationRecoveryService{}
	manager := NewRunManager(database, nil, nil)
	preserved, err := manager.recoverPublicationRuns(context.Background(), service)
	if err != nil {
		t.Fatalf("recover publication runs: %v", err)
	}
	if _, ok := preserved[publicationRun.ID]; !ok || len(preserved) != 1 {
		t.Fatalf("preserved publication runs = %#v, want only %s", preserved, publicationRun.ID)
	}
	if len(service.calls) != 1 || service.calls[0] != (publicationRecoveryCall{operation: "resume", publicationID: publicationRow.PublicationID}) {
		t.Fatalf("publication recovery calls = %#v, want one resume", service.calls)
	}

	count, err := database.RecoverStaleRunsExcept("daemon crashed during execution", preserved)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("ordinary stale recovery count = %d, want 1", count)
	}
	gotPublication, err := database.GetRun(publicationRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotOrdinary, err := database.GetRun(ordinaryRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotPublication.Status != types.RunRunning || gotPublication.Kind != db.RunKindFactoryPublicationV1 {
		t.Fatalf("publication run entered ordinary stale failure: %#v", gotPublication)
	}
	if gotOrdinary.Status != types.RunFailed || gotOrdinary.Kind != db.RunKindStandard {
		t.Fatalf("ordinary stale semantics changed: %#v", gotOrdinary)
	}
}

func TestPublicationStartupRecoveryRepeatsInterruptedDefenseFromFreshPendingState(t *testing.T) {
	for _, interrupted := range []types.StepName{
		types.StepIntent,
		types.StepRebase,
		types.StepReview,
		types.StepTest,
		types.StepDocument,
		types.StepLint,
	} {
		t.Run(string(interrupted), func(t *testing.T) {
			database := openPublicationRecoveryDB(t)
			publicationRow, run := createDaemonRecoveryPublication(t, database, "defense-"+string(interrupted))
			for _, name := range types.AllSteps() {
				step := publicationStep(t, database, run.ID, name)
				if name.Order() < interrupted.Order() {
					if err := database.CompleteStep(step.ID, 0, 1, ""); err != nil {
						t.Fatalf("complete prior %s: %v", name, err)
					}
				}
			}
			interruptedStep := publicationStep(t, database, run.ID, interrupted)
			if err := database.StartStep(interruptedStep.ID); err != nil {
				t.Fatalf("start interrupted %s: %v", interrupted, err)
			}

			service := &fakePublicationRecoveryService{
				resumeCheck: func(publicationID string) error {
					if publicationID != publicationRow.PublicationID {
						return fmt.Errorf("resumed publication %s, want %s", publicationID, publicationRow.PublicationID)
					}
					fresh := publicationStep(t, database, run.ID, interrupted)
					if fresh.Status != types.StepStatusPending || fresh.StartedAt != nil {
						return fmt.Errorf("interrupted %s was not reset fresh: %#v", interrupted, fresh)
					}
					return nil
				},
			}
			manager := NewRunManager(database, nil, nil)
			if _, err := manager.recoverPublicationRuns(context.Background(), service); err != nil {
				t.Fatalf("recover publication defense: %v", err)
			}
			if len(service.calls) != 1 || service.calls[0].operation != "resume" {
				t.Fatalf("defense recovery calls = %#v, want one fresh resume", service.calls)
			}
		})
	}
}

func TestPublicationStartupRecoveryPreservesObservedCIForExecutorCompletion(t *testing.T) {
	database := openPublicationRecoveryDB(t)
	publicationRow, run := createDaemonRecoveryPublication(t, database, "observed-ci")
	for _, name := range types.AllSteps() {
		step := publicationStep(t, database, run.ID, name)
		if name == types.StepCI {
			if err := database.StartStep(step.ID); err != nil {
				t.Fatalf("start interrupted CI: %v", err)
			}
			continue
		}
		if err := database.CompleteStep(step.ID, 0, 1, ""); err != nil {
			t.Fatalf("complete prior %s: %v", name, err)
		}
	}
	binding := recoveryEffectBinding(db.PublicationEffectCI)
	if _, err := database.PlanPublicationEffect(db.PlanPublicationEffectInput{
		PublicationID: publicationRow.PublicationID,
		Kind:          db.PublicationEffectCI,
		Binding:       binding,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginPublicationEffect(db.BeginPublicationEffectInput{
		PublicationID: publicationRow.PublicationID,
		Kind:          db.PublicationEffectCI,
		Binding:       binding,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ConcludePublicationEffect(db.ConcludePublicationEffectInput{
		PublicationID: publicationRow.PublicationID,
		Kind:          db.PublicationEffectCI,
		Binding:       binding,
		State:         db.PublicationEffectObserved,
		Observation:   []byte(`{"head":"1111111111111111111111111111111111111111","checks":["PASS"]}`),
	}); err != nil {
		t.Fatal(err)
	}

	service := &fakePublicationRecoveryService{
		resumeCheck: func(publicationID string) error {
			if publicationID != publicationRow.PublicationID {
				return fmt.Errorf("resumed publication %s, want %s", publicationID, publicationRow.PublicationID)
			}
			ci := publicationStep(t, database, run.ID, types.StepCI)
			if ci.Status != types.StepStatusRunning {
				return fmt.Errorf("observed CI was reset to %s; Executor must consume its durable observation", ci.Status)
			}
			return nil
		},
	}
	manager := NewRunManager(database, nil, nil)
	if _, err := manager.recoverPublicationRuns(context.Background(), service); err != nil {
		t.Fatalf("recover observed CI: %v", err)
	}
	if len(service.calls) != 1 || service.calls[0].operation != "resume" {
		t.Fatalf("observed CI recovery calls = %#v, want one Executor resume", service.calls)
	}
}

func TestPublicationStartupRecoveryDoesNotConsumeUnusedEffectDecision(t *testing.T) {
	for _, authorized := range []bool{false, true} {
		name := "planned"
		if authorized {
			name = "authorized"
		}
		t.Run(name, func(t *testing.T) {
			database := openPublicationRecoveryDB(t)
			publicationRow, _ := createDaemonRecoveryPublication(t, database, "unused-"+name)
			binding := recoveryEffectBinding(db.PublicationEffectPush)
			if _, err := database.PlanPublicationEffect(db.PlanPublicationEffectInput{
				PublicationID: publicationRow.PublicationID,
				Kind:          db.PublicationEffectPush,
				Binding:       binding,
			}); err != nil {
				t.Fatal(err)
			}
			if authorized {
				if _, err := database.AuthorizePublicationEffect(db.AuthorizePublicationEffectInput{
					PublicationID:  publicationRow.PublicationID,
					Kind:           db.PublicationEffectPush,
					Binding:        binding,
					DecisionDigest: "decision-unused",
				}); err != nil {
					t.Fatal(err)
				}
			}

			service := &fakePublicationRecoveryService{}
			manager := NewRunManager(database, nil, nil)
			if _, err := manager.recoverPublicationRuns(context.Background(), service); err != nil {
				t.Fatalf("recover unused effect: %v", err)
			}
			if len(service.calls) != 1 || service.calls[0].operation != "resume" {
				t.Fatalf("unused effect recovery calls = %#v, want resume without reconcile", service.calls)
			}
			effect, err := database.GetPublicationEffect(publicationRow.PublicationID, db.PublicationEffectPush)
			if err != nil {
				t.Fatal(err)
			}
			if effect.EffectStartedAt != nil || effect.DecisionConsumedAt != nil {
				t.Fatalf("startup consumed an unused decision/effect: %#v", effect)
			}
			wantState := db.PublicationEffectPlanned
			if authorized {
				wantState = db.PublicationEffectAuthorized
			}
			if effect.State != wantState {
				t.Fatalf("unused effect state = %s, want %s", effect.State, wantState)
			}
		})
	}
}

func TestPublicationStartupRecoveryReconcilesMaybeEffectBeforeAnyResume(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		dbKind db.PublicationEffectKind
		kind   publication.EffectKind
	}{
		{name: "push", dbKind: db.PublicationEffectPush, kind: publication.EffectPush},
		{name: "pr", dbKind: db.PublicationEffectPR, kind: publication.EffectPR},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			database := openPublicationRecoveryDB(t)
			publicationRow, _ := createDaemonRecoveryPublication(t, database, "started-"+testCase.name)
			binding := recoveryEffectBinding(testCase.dbKind)
			payload := []byte(nil)
			if testCase.dbKind == db.PublicationEffectPR {
				payload = []byte("exact persisted PR draft")
			}
			if _, err := database.PlanPublicationEffect(db.PlanPublicationEffectInput{
				PublicationID:   publicationRow.PublicationID,
				Kind:            testCase.dbKind,
				Binding:         binding,
				PreparedPayload: payload,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := database.AuthorizePublicationEffect(db.AuthorizePublicationEffectInput{
				PublicationID:  publicationRow.PublicationID,
				Kind:           testCase.dbKind,
				Binding:        binding,
				DecisionDigest: "decision-started",
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := database.BeginPublicationEffect(db.BeginPublicationEffectInput{
				PublicationID:  publicationRow.PublicationID,
				Kind:           testCase.dbKind,
				Binding:        binding,
				DecisionDigest: "decision-started",
			}); err != nil {
				t.Fatal(err)
			}

			service := &fakePublicationRecoveryService{
				recover: func(publicationID string, kind publication.EffectKind) (publication.Result, error) {
					if publicationID != publicationRow.PublicationID || kind != testCase.kind {
						return publication.Result{}, fmt.Errorf("wrong reconcile target %s/%s", publicationID, kind)
					}
					if _, err := database.ConcludePublicationEffect(db.ConcludePublicationEffectInput{
						PublicationID: publicationID,
						Kind:          testCase.dbKind,
						Binding:       binding,
						State:         db.PublicationEffectObserved,
						Observation:   []byte(`{"exact":true}`),
					}); err != nil {
						return publication.Result{}, err
					}
					return publication.Result{Protocol: publication.ProtocolV1, PublicationID: publicationID, Status: publication.StatusReadyForPR}, nil
				},
			}
			manager := NewRunManager(database, nil, nil)
			if _, err := manager.recoverPublicationRuns(context.Background(), service); err != nil {
				t.Fatalf("recover started effect: %v", err)
			}
			if len(service.calls) != 1 || service.calls[0] != (publicationRecoveryCall{operation: "reconcile", publicationID: publicationRow.PublicationID, kind: testCase.kind}) {
				t.Fatalf("started effect calls = %#v, want reconcile first and no replay/resume", service.calls)
			}
		})
	}
}

func TestPublicationStartupRecoveryAmbiguityIsEffectUnknownWithoutReplay(t *testing.T) {
	database := openPublicationRecoveryDB(t)
	publicationRow, _ := createDaemonRecoveryPublication(t, database, "ambiguous")
	binding := recoveryEffectBinding(db.PublicationEffectPR)
	if _, err := database.PlanPublicationEffect(db.PlanPublicationEffectInput{
		PublicationID:   publicationRow.PublicationID,
		Kind:            db.PublicationEffectPR,
		Binding:         binding,
		PreparedPayload: []byte("exact persisted PR draft"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.AuthorizePublicationEffect(db.AuthorizePublicationEffectInput{
		PublicationID:  publicationRow.PublicationID,
		Kind:           db.PublicationEffectPR,
		Binding:        binding,
		DecisionDigest: "decision-ambiguous",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginPublicationEffect(db.BeginPublicationEffectInput{
		PublicationID:  publicationRow.PublicationID,
		Kind:           db.PublicationEffectPR,
		Binding:        binding,
		DecisionDigest: "decision-ambiguous",
	}); err != nil {
		t.Fatal(err)
	}

	service := &fakePublicationRecoveryService{
		recover: func(publicationID string, kind publication.EffectKind) (publication.Result, error) {
			if _, err := database.ConcludePublicationEffect(db.ConcludePublicationEffectInput{
				PublicationID: publicationID,
				Kind:          db.PublicationEffectPR,
				Binding:       binding,
				State:         db.PublicationEffectUnknown,
				Observation:   []byte(`{"matches":2}`),
			}); err != nil {
				return publication.Result{}, err
			}
			return publication.Result{Protocol: publication.ProtocolV1, PublicationID: publicationID, Status: publication.StatusEffectUnknown}, nil
		},
	}
	manager := NewRunManager(database, nil, nil)
	if _, err := manager.recoverPublicationRuns(context.Background(), service); err != nil {
		t.Fatalf("recover ambiguous PR effect: %v", err)
	}
	if len(service.calls) != 1 || service.calls[0] != (publicationRecoveryCall{operation: "reconcile", publicationID: publicationRow.PublicationID, kind: publication.EffectPR}) {
		t.Fatalf("ambiguous effect calls = %#v, want one reconcile and no replay/resume", service.calls)
	}
	effect, err := database.GetPublicationEffect(publicationRow.PublicationID, db.PublicationEffectPR)
	if err != nil {
		t.Fatal(err)
	}
	if effect.State != db.PublicationEffectUnknown {
		t.Fatalf("ambiguous PR recovery state = %s, want %s", effect.State, db.PublicationEffectUnknown)
	}
}
