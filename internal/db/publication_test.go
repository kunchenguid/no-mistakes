package db

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func publicationInput(repoID, candidateRef, suffix string) CreatePublicationInput {
	request := []byte(fmt.Sprintf(`{"protocol":"factory-publication-v1","case":%q}`, suffix))
	digest := sha256.Sum256(request)
	return CreatePublicationInput{
		PublicationID:    fmt.Sprintf("%x", digest),
		CanonicalRequest: request,
		RepoID:           repoID,
		CandidateRef:     candidateRef,
		BaseRef:          "refs/heads/main",
		HeadSHA:          "1111111111111111111111111111111111111111",
		BaseSHA:          "0000000000000000000000000000000000000000",
		TreeSHA:          "2222222222222222222222222222222222222222",
	}
}

func insertPublicationRepo(t *testing.T, d *DB, id string) *Repo {
	t.Helper()
	repo, err := d.InsertRepoWithID(
		id,
		"/work/"+id,
		"https://github.com/example/"+id+".git",
		"main",
	)
	if err != nil {
		t.Fatalf("insert repository: %v", err)
	}
	return repo
}

func createPublication(t *testing.T, d *DB, input CreatePublicationInput) (*Publication, *Run) {
	t.Helper()
	publication, run, created, err := d.CreateOrGetPublication(input)
	if err != nil {
		t.Fatalf("create publication: %v", err)
	}
	if !created {
		t.Fatal("first CreateOrGetPublication call reported an existing publication")
	}
	if publication == nil || run == nil {
		t.Fatalf("CreateOrGetPublication returned publication=%#v run=%#v", publication, run)
	}
	return publication, run
}

func TestPublicationSchemaUsesClosedRunKindsAndMigratesStandardRuns(t *testing.T) {
	d := openTestDB(t)
	repo := insertPublicationRepo(t, d, "repo-schema")

	standard, err := d.InsertRun(repo.ID, "feature/standard", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if standard.Kind != RunKindStandard {
		t.Fatalf("ordinary InsertRun kind = %q, want %q", standard.Kind, RunKindStandard)
	}

	input := publicationInput(repo.ID, "refs/heads/feature/publication", "schema")
	_, publicationRun := createPublication(t, d, input)
	if publicationRun.Kind != RunKindFactoryPublicationV1 {
		t.Fatalf("publication run kind = %q, want %q", publicationRun.Kind, RunKindFactoryPublicationV1)
	}

	for _, table := range []string{"publications", "publication_effects"} {
		var count int
		if err := d.sql.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatalf("%s table is missing: %v", table, err)
		}
	}
	if !hasColumn(t, d, "runs", "run_kind") {
		t.Fatal("runs.run_kind column is missing")
	}

	_, err = d.sql.Exec(
		`INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, run_kind, status, created_at, updated_at)
		 VALUES ('invalid-kind', ?, 'bad', 'head', 'base', 'other', 'pending', 1, 1)`,
		repo.ID,
	)
	if err == nil {
		t.Fatal("runs.run_kind accepted a value outside standard|factory-publication-v1")
	}
}

func TestOpenMigratesLegacyRunsToStandardKindWithoutInference(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.sqlite")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
		CREATE TABLE repos (id TEXT PRIMARY KEY, working_path TEXT NOT NULL UNIQUE, upstream_url TEXT NOT NULL, default_branch TEXT NOT NULL DEFAULT 'main', created_at INTEGER NOT NULL);
		CREATE TABLE runs (id TEXT PRIMARY KEY, repo_id TEXT NOT NULL, branch TEXT NOT NULL, head_sha TEXT NOT NULL, base_sha TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', pr_url TEXT, error TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
		INSERT INTO repos VALUES ('repo-legacy-publication', '/work/legacy-publication', 'https://github.com/example/legacy.git', 'main', 1);
		INSERT INTO runs VALUES ('run-legacy-publication', 'repo-legacy-publication', 'feature', 'head', 'base', 'completed', NULL, NULL, 1, 1);
	`)
	if err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	run, err := d.GetRun("run-legacy-publication")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.Kind != RunKindStandard {
		t.Fatalf("legacy run kind = %#v, want standard", run)
	}
	var publicationCount int
	if err := d.sql.QueryRow(`SELECT count(*) FROM publications WHERE run_id = ?`, run.ID).Scan(&publicationCount); err != nil {
		t.Fatal(err)
	}
	if publicationCount != 0 {
		t.Fatalf("legacy ordinary run gained %d inferred publication bindings", publicationCount)
	}
}

func TestCreatePublicationIsAtomicAndRoundTripsExactBytes(t *testing.T) {
	d := openTestDB(t)
	repo := insertPublicationRepo(t, d, "repo-atomic")
	input := publicationInput(repo.ID, "refs/heads/feature/atomic", "atomic")
	wantBytes := append([]byte(nil), input.CanonicalRequest...)

	publication, run := createPublication(t, d, input)
	if publication.PublicationID != input.PublicationID || publication.RunID != run.ID {
		t.Fatalf("publication association = %#v, run = %#v", publication, run)
	}
	if publication.RepoID != input.RepoID ||
		publication.CandidateRef != input.CandidateRef ||
		publication.BaseRef != input.BaseRef ||
		publication.HeadSHA != input.HeadSHA ||
		publication.BaseSHA != input.BaseSHA ||
		publication.TreeSHA != input.TreeSHA {
		t.Fatalf("publication lost an exact candidate binding: %#v", publication)
	}
	if !bytes.Equal(publication.CanonicalRequest, wantBytes) {
		t.Fatalf("canonical request = %q, want exact bytes %q", publication.CanonicalRequest, wantBytes)
	}

	// Callers retain no mutable alias to the durable request bytes.
	input.CanonicalRequest[0] ^= 0xff
	stored, err := d.GetPublication(publication.PublicationID)
	if err != nil {
		t.Fatalf("get publication: %v", err)
	}
	if stored == nil || !bytes.Equal(stored.CanonicalRequest, wantBytes) {
		t.Fatalf("stored canonical request changed through caller alias: %#v", stored)
	}

	bad := publicationInput("missing-repository", "refs/heads/feature/missing", "rollback")
	if _, _, _, err := d.CreateOrGetPublication(bad); err == nil {
		t.Fatal("publication creation with a missing repository succeeded")
	}
	var publications int
	if err := d.sql.QueryRow(`SELECT count(*) FROM publications WHERE publication_id = ?`, bad.PublicationID).Scan(&publications); err != nil {
		t.Fatal(err)
	}
	var runs int
	if err := d.sql.QueryRow(`SELECT count(*) FROM runs WHERE repo_id = ? AND branch = ?`, bad.RepoID, bad.CandidateRef).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if publications != 0 || runs != 0 {
		t.Fatalf("failed publication admission left publication=%d run=%d", publications, runs)
	}
}

func TestCreatePublicationIsIdempotentOnlyForIdenticalCanonicalBytes(t *testing.T) {
	d := openTestDB(t)
	repo := insertPublicationRepo(t, d, "repo-idempotent")
	input := publicationInput(repo.ID, "refs/heads/feature/idempotent", "same")
	firstPublication, firstRun := createPublication(t, d, input)

	secondPublication, secondRun, created, err := d.CreateOrGetPublication(input)
	if err != nil {
		t.Fatalf("reconcile identical publication: %v", err)
	}
	if created {
		t.Fatal("identical publication created a second association")
	}
	if secondPublication.RunID != firstPublication.RunID || secondRun.ID != firstRun.ID {
		t.Fatalf("identical publication reconciled to publication=%s run=%s, want run %s", secondPublication.RunID, secondRun.ID, firstRun.ID)
	}

	changed := input
	changed.CanonicalRequest = append([]byte(nil), input.CanonicalRequest...)
	changed.CanonicalRequest[len(changed.CanonicalRequest)-2] ^= 1
	_, _, _, err = d.CreateOrGetPublication(changed)
	if !errors.Is(err, ErrPublicationCollision) {
		t.Fatalf("same publication id with different bytes error = %v, want ErrPublicationCollision", err)
	}

	wrongID := input
	wrongID.PublicationID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, _, _, err = d.CreateOrGetPublication(wrongID)
	if !errors.Is(err, ErrPublicationIDMismatch) {
		t.Fatalf("publication id not matching request bytes error = %v, want ErrPublicationIDMismatch", err)
	}

	changedCandidate := input
	changedCandidate.HeadSHA = "3333333333333333333333333333333333333333"
	_, _, _, err = d.CreateOrGetPublication(changedCandidate)
	if !errors.Is(err, ErrPublicationCollision) {
		t.Fatalf("same request identity with a different candidate error = %v, want ErrPublicationCollision", err)
	}

	concurrentCandidate := publicationInput(repo.ID, input.CandidateRef, "different-publication")
	_, _, _, err = d.CreateOrGetPublication(concurrentCandidate)
	if !errors.Is(err, ErrPublicationRunConflict) {
		t.Fatalf("second active publication for the same repository/ref error = %v, want ErrPublicationRunConflict", err)
	}
}

func TestPublicationNeverAttachesAnOrdinaryRun(t *testing.T) {
	d := openTestDB(t)
	repo := insertPublicationRepo(t, d, "repo-standard-conflict")
	input := publicationInput(repo.ID, "refs/heads/feature/shared", "standard-conflict")
	standard, err := d.InsertRun(repo.ID, input.CandidateRef, input.HeadSHA, input.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}

	_, _, _, err = d.CreateOrGetPublication(input)
	if !errors.Is(err, ErrPublicationRunConflict) {
		t.Fatalf("admission beside an active ordinary run error = %v, want ErrPublicationRunConflict", err)
	}
	got, err := d.GetRun(standard.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Kind != RunKindStandard {
		t.Fatalf("ordinary run was retyped or replaced: %#v", got)
	}
	var count int
	if err := d.sql.QueryRow(`SELECT count(*) FROM publications`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("ordinary-run conflict persisted %d publication bindings", count)
	}
}

func TestConcurrentIdenticalPublicationCreatesExactlyOneRun(t *testing.T) {
	d := openTestDB(t)
	repo := insertPublicationRepo(t, d, "repo-concurrent")
	input := publicationInput(repo.ID, "refs/heads/feature/concurrent", "concurrent")

	const callers = 12
	type result struct {
		publication *Publication
		run         *Run
		created     bool
		err         error
	}
	start := make(chan struct{})
	results := make(chan result, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			publication, run, created, err := d.CreateOrGetPublication(input)
			results <- result{publication: publication, run: run, created: created, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var runID string
	createdCount := 0
	for result := range results {
		if result.err != nil {
			t.Errorf("concurrent CreateOrGetPublication: %v", result.err)
			continue
		}
		if result.publication == nil || result.run == nil {
			t.Errorf("concurrent result contains nil: %#v", result)
			continue
		}
		if result.created {
			createdCount++
		}
		if runID == "" {
			runID = result.run.ID
		}
		if result.run.ID != runID || result.publication.RunID != runID {
			t.Errorf("concurrent request attached to run %q/%q, want %q", result.run.ID, result.publication.RunID, runID)
		}
	}
	if createdCount != 1 {
		t.Fatalf("created result count = %d, want exactly 1", createdCount)
	}
	var runCount, publicationCount int
	if err := d.sql.QueryRow(`SELECT count(*) FROM runs WHERE run_kind = ?`, RunKindFactoryPublicationV1).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if err := d.sql.QueryRow(`SELECT count(*) FROM publications WHERE publication_id = ?`, input.PublicationID).Scan(&publicationCount); err != nil {
		t.Fatal(err)
	}
	if runCount != 1 || publicationCount != 1 {
		t.Fatalf("durable concurrent result = %d runs, %d publications; want 1 and 1", runCount, publicationCount)
	}
}

func publicationEffectBinding(kind PublicationEffectKind, suffix string) PublicationEffectBinding {
	binding := PublicationEffectBinding{
		CandidateSHA:   "1111111111111111111111111111111111111111",
		RemoteIdentity: "github.com/example/repository",
		DestinationRef: "refs/heads/feature/publication",
		HeadRef:        "refs/heads/feature/publication",
		EffectDigest:   fmt.Sprintf("effect-%s-%s", kind, suffix),
	}
	if kind == PublicationEffectPR || kind == PublicationEffectCI {
		binding.BaseRef = "refs/heads/main"
	}
	if kind == PublicationEffectPR {
		binding.DraftDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}
	return binding
}

func planEffect(t *testing.T, d *DB, publicationID string, kind PublicationEffectKind, suffix string) *PublicationEffect {
	t.Helper()
	effect, err := d.PlanPublicationEffect(PlanPublicationEffectInput{
		PublicationID: publicationID,
		Kind:          kind,
		Binding:       publicationEffectBinding(kind, suffix),
	})
	if err != nil {
		t.Fatalf("plan %s effect: %v", kind, err)
	}
	if effect == nil || effect.State != PublicationEffectPlanned {
		t.Fatalf("planned %s effect = %#v", kind, effect)
	}
	return effect
}

func TestPublicationEffectsPersistClosedKindsStatesAndExactBindings(t *testing.T) {
	d := openTestDB(t)
	repo := insertPublicationRepo(t, d, "repo-effects")
	input := publicationInput(repo.ID, "refs/heads/feature/effects", "effects")
	publication, _ := createPublication(t, d, input)

	for _, kind := range []PublicationEffectKind{PublicationEffectPush, PublicationEffectPR, PublicationEffectCI} {
		effect := planEffect(t, d, publication.PublicationID, kind, "closed")
		stored, err := d.GetPublicationEffect(publication.PublicationID, kind)
		if err != nil {
			t.Fatalf("get %s effect: %v", kind, err)
		}
		if stored == nil || stored.PublicationID != publication.PublicationID || stored.Kind != kind || stored.State != PublicationEffectPlanned || stored.Binding != effect.Binding {
			t.Errorf("stored %s effect = %#v, want exact planned binding %#v", kind, stored, effect.Binding)
		}
	}

	_, err := d.sql.Exec(
		`INSERT INTO publication_effects (publication_id, effect_kind, effect_state, candidate_sha, remote_identity, destination_ref, effect_digest, created_at, updated_at)
		 VALUES (?, 'deploy', 'planned', 'h', 'r', 'd', 'e', 1, 1)`,
		publication.PublicationID,
	)
	if err == nil {
		t.Fatal("publication_effects accepted an effect kind outside push|pr|ci")
	}
	_, err = d.sql.Exec(
		`UPDATE publication_effects SET effect_state = 'skipped' WHERE publication_id = ? AND effect_kind = ?`,
		publication.PublicationID,
		PublicationEffectPush,
	)
	if err == nil {
		t.Fatal("publication_effects accepted a state outside planned|authorized|observed|unknown|failed")
	}
}

func TestPublicationEffectPlanIsIdempotentButBindingCollisionFails(t *testing.T) {
	d := openTestDB(t)
	repo := insertPublicationRepo(t, d, "repo-effect-plan")
	input := publicationInput(repo.ID, "refs/heads/feature/effect-plan", "effect-plan")
	publication, _ := createPublication(t, d, input)
	binding := publicationEffectBinding(PublicationEffectPush, "same")

	first, err := d.PlanPublicationEffect(PlanPublicationEffectInput{PublicationID: publication.PublicationID, Kind: PublicationEffectPush, Binding: binding})
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.PlanPublicationEffect(PlanPublicationEffectInput{PublicationID: publication.PublicationID, Kind: PublicationEffectPush, Binding: binding})
	if err != nil {
		t.Fatalf("idempotent plan: %v", err)
	}
	if first.ID != second.ID || first.Binding != second.Binding {
		t.Fatalf("identical plan reconciled to %#v, want %#v", second, first)
	}

	changed := binding
	changed.DestinationRef = "refs/heads/other"
	_, err = d.PlanPublicationEffect(PlanPublicationEffectInput{PublicationID: publication.PublicationID, Kind: PublicationEffectPush, Binding: changed})
	if !errors.Is(err, ErrPublicationEffectConflict) {
		t.Fatalf("changed binding error = %v, want ErrPublicationEffectConflict", err)
	}
}

func TestPushAndPRAuthorizationIsExactDurableAndSingleUse(t *testing.T) {
	for _, kind := range []PublicationEffectKind{PublicationEffectPush, PublicationEffectPR} {
		t.Run(string(kind), func(t *testing.T) {
			d := openTestDB(t)
			repo := insertPublicationRepo(t, d, "repo-authorize-"+string(kind))
			input := publicationInput(repo.ID, "refs/heads/feature/authorize-"+string(kind), "authorize-"+string(kind))
			publication, _ := createPublication(t, d, input)
			effect := planEffect(t, d, publication.PublicationID, kind, "authorize")

			_, err := d.BeginPublicationEffect(BeginPublicationEffectInput{
				PublicationID:  publication.PublicationID,
				Kind:           kind,
				Binding:        effect.Binding,
				DecisionDigest: "decision-1",
			})
			if !errors.Is(err, ErrPublicationAuthorizationRequired) {
				t.Fatalf("begin before authorization error = %v, want ErrPublicationAuthorizationRequired", err)
			}

			changed := effect.Binding
			if kind == PublicationEffectPush {
				changed.RemoteIdentity = "github.com/other/repository"
			} else {
				changed.DraftDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			}
			_, err = d.AuthorizePublicationEffect(AuthorizePublicationEffectInput{
				PublicationID:  publication.PublicationID,
				Kind:           kind,
				Binding:        changed,
				DecisionDigest: "decision-1",
			})
			if !errors.Is(err, ErrPublicationEffectConflict) {
				t.Fatalf("authorize with changed binding error = %v, want ErrPublicationEffectConflict", err)
			}

			authorized, err := d.AuthorizePublicationEffect(AuthorizePublicationEffectInput{
				PublicationID:  publication.PublicationID,
				Kind:           kind,
				Binding:        effect.Binding,
				DecisionDigest: "decision-1",
			})
			if err != nil {
				t.Fatalf("authorize effect: %v", err)
			}
			if authorized.State != PublicationEffectAuthorized || authorized.DecisionDigest == nil || *authorized.DecisionDigest != "decision-1" || authorized.DecisionConsumedAt != nil {
				t.Fatalf("authorized effect = %#v", authorized)
			}

			_, err = d.BeginPublicationEffect(BeginPublicationEffectInput{
				PublicationID:  publication.PublicationID,
				Kind:           kind,
				Binding:        effect.Binding,
				DecisionDigest: "decision-other",
			})
			if !errors.Is(err, ErrPublicationAuthorizationMismatch) {
				t.Fatalf("begin with a different decision error = %v, want ErrPublicationAuthorizationMismatch", err)
			}

			started, err := d.BeginPublicationEffect(BeginPublicationEffectInput{
				PublicationID:  publication.PublicationID,
				Kind:           kind,
				Binding:        effect.Binding,
				DecisionDigest: "decision-1",
			})
			if err != nil {
				t.Fatalf("begin authorized effect: %v", err)
			}
			if started.DecisionConsumedAt == nil || started.EffectStartedAt == nil {
				t.Fatalf("begin did not durably consume decision before effect: %#v", started)
			}

			_, err = d.BeginPublicationEffect(BeginPublicationEffectInput{
				PublicationID:  publication.PublicationID,
				Kind:           kind,
				Binding:        effect.Binding,
				DecisionDigest: "decision-1",
			})
			if !errors.Is(err, ErrPublicationDecisionConsumed) {
				t.Fatalf("second begin error = %v, want ErrPublicationDecisionConsumed", err)
			}
		})
	}
}

func TestPublicationEffectDenyIsExactAtomicTerminalAndIdempotent(t *testing.T) {
	for _, kind := range []PublicationEffectKind{PublicationEffectPush, PublicationEffectPR} {
		t.Run(string(kind), func(t *testing.T) {
			d := openTestDB(t)
			repo := insertPublicationRepo(t, d, "repo-deny-"+string(kind))
			input := publicationInput(repo.ID, "refs/heads/feature/deny-"+string(kind), "deny-"+string(kind))
			publication, run := createPublication(t, d, input)
			effect := planEffect(t, d, publication.PublicationID, kind, "deny")
			decisionDigest := strings.Repeat("d", 64)

			denied, err := d.DenyPublicationEffect(DenyPublicationEffectInput{
				PublicationID:  publication.PublicationID,
				Kind:           kind,
				Binding:        effect.Binding,
				DecisionDigest: decisionDigest,
			})
			if err != nil {
				t.Fatalf("deny exact effect: %v", err)
			}
			if denied.State != PublicationEffectFailed || denied.DecisionDigest == nil || *denied.DecisionDigest != decisionDigest ||
				denied.DecisionConsumedAt == nil || denied.EffectStartedAt != nil || denied.ObservedAt != nil || len(denied.Observation) != 0 {
				t.Fatalf("denied effect did not terminalize without provider start: %#v", denied)
			}
			consumedAt := *denied.DecisionConsumedAt
			failedRun, err := d.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if failedRun.Status != types.RunFailed || failedRun.Error == nil ||
				!strings.HasPrefix(*failedRun.Error, PublicationDenialErrorPrefix) || !strings.Contains(*failedRun.Error, string(kind)) {
				t.Fatalf("DENY and Run failure were not atomically bound: %#v", failedRun)
			}

			retried, err := d.DenyPublicationEffect(DenyPublicationEffectInput{
				PublicationID:  publication.PublicationID,
				Kind:           kind,
				Binding:        effect.Binding,
				DecisionDigest: decisionDigest,
			})
			if err != nil {
				t.Fatalf("idempotent exact DENY retry: %v", err)
			}
			if retried.ID != denied.ID || retried.DecisionConsumedAt == nil || *retried.DecisionConsumedAt != consumedAt || retried.EffectStartedAt != nil {
				t.Fatalf("DENY retry changed durable effect: %#v, want %#v", retried, denied)
			}

			_, err = d.DenyPublicationEffect(DenyPublicationEffectInput{
				PublicationID:  publication.PublicationID,
				Kind:           kind,
				Binding:        effect.Binding,
				DecisionDigest: strings.Repeat("e", 64),
			})
			if !errors.Is(err, ErrPublicationAuthorizationMismatch) {
				t.Fatalf("stale DENY retry error = %v, want ErrPublicationAuthorizationMismatch", err)
			}
			unchanged, _ := d.GetPublicationEffect(publication.PublicationID, kind)
			if unchanged.DecisionConsumedAt == nil || *unchanged.DecisionConsumedAt != consumedAt || unchanged.EffectStartedAt != nil {
				t.Fatalf("stale DENY retry changed effect: %#v", unchanged)
			}

			_, err = d.AuthorizePublicationEffect(AuthorizePublicationEffectInput{
				PublicationID:  publication.PublicationID,
				Kind:           kind,
				Binding:        effect.Binding,
				DecisionDigest: "go-after-deny",
			})
			if !errors.Is(err, ErrPublicationDecisionConsumed) {
				t.Fatalf("DENY transferred to GO: %v", err)
			}
		})
	}
}

func TestPublicationEffectDenyMismatchForeignKindAndGOStateHaveNoEffect(t *testing.T) {
	t.Run("mismatch and foreign kind", func(t *testing.T) {
		d := openTestDB(t)
		repo := insertPublicationRepo(t, d, "repo-deny-mismatch")
		publication, run := createPublication(t, d, publicationInput(repo.ID, "refs/heads/feature/deny-mismatch", "deny-mismatch"))
		effect := planEffect(t, d, publication.PublicationID, PublicationEffectPush, "deny-mismatch")
		changed := effect.Binding
		changed.DestinationRef = "refs/heads/foreign"

		for name, input := range map[string]DenyPublicationEffectInput{
			"binding": {
				PublicationID: publication.PublicationID, Kind: PublicationEffectPush, Binding: changed, DecisionDigest: strings.Repeat("d", 64),
			},
			"empty decision": {
				PublicationID: publication.PublicationID, Kind: PublicationEffectPush, Binding: effect.Binding,
			},
			"foreign kind": {
				PublicationID: publication.PublicationID, Kind: PublicationEffectPR, Binding: effect.Binding, DecisionDigest: strings.Repeat("d", 64),
			},
			"CI kind": {
				PublicationID: publication.PublicationID, Kind: PublicationEffectCI, Binding: effect.Binding, DecisionDigest: strings.Repeat("d", 64),
			},
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := d.DenyPublicationEffect(input); err == nil {
					t.Fatalf("DENY accepted %s input", name)
				}
				stored, _ := d.GetPublicationEffect(publication.PublicationID, PublicationEffectPush)
				storedRun, _ := d.GetRun(run.ID)
				if stored.State != PublicationEffectPlanned || stored.DecisionDigest != nil || stored.DecisionConsumedAt != nil || stored.EffectStartedAt != nil {
					t.Fatalf("rejected %s DENY changed effect: %#v", name, stored)
				}
				if storedRun.Status != types.RunPending || storedRun.Error != nil {
					t.Fatalf("rejected %s DENY changed Run: %#v", name, storedRun)
				}
			})
		}
	})

	t.Run("GO cannot become DENY", func(t *testing.T) {
		d := openTestDB(t)
		repo := insertPublicationRepo(t, d, "repo-go-to-deny")
		publication, run := createPublication(t, d, publicationInput(repo.ID, "refs/heads/feature/go-to-deny", "go-to-deny"))
		effect := planEffect(t, d, publication.PublicationID, PublicationEffectPush, "go-to-deny")
		authorized, err := d.AuthorizePublicationEffect(AuthorizePublicationEffectInput{
			PublicationID: publication.PublicationID, Kind: PublicationEffectPush, Binding: effect.Binding, DecisionDigest: "go-decision",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.DenyPublicationEffect(DenyPublicationEffectInput{
			PublicationID: publication.PublicationID, Kind: PublicationEffectPush, Binding: effect.Binding, DecisionDigest: strings.Repeat("d", 64),
		}); err == nil {
			t.Fatal("authorized GO transferred to DENY")
		}
		stored, _ := d.GetPublicationEffect(publication.PublicationID, PublicationEffectPush)
		storedRun, _ := d.GetRun(run.ID)
		if stored.State != PublicationEffectAuthorized || stored.DecisionDigest == nil || *stored.DecisionDigest != *authorized.DecisionDigest || stored.DecisionConsumedAt != nil {
			t.Fatalf("rejected GO-to-DENY changed effect: %#v", stored)
		}
		if storedRun.Status != types.RunPending || storedRun.Error != nil {
			t.Fatalf("rejected GO-to-DENY changed Run: %#v", storedRun)
		}
	})
}

func TestPublicationEffectTerminalJournalStatesAreFailClosed(t *testing.T) {
	for _, terminal := range []PublicationEffectState{
		PublicationEffectObserved,
		PublicationEffectUnknown,
		PublicationEffectFailed,
	} {
		t.Run(string(terminal), func(t *testing.T) {
			d := openTestDB(t)
			repo := insertPublicationRepo(t, d, "repo-terminal-"+string(terminal))
			input := publicationInput(repo.ID, "refs/heads/feature/terminal-"+string(terminal), "terminal-"+string(terminal))
			publication, _ := createPublication(t, d, input)
			effect := planEffect(t, d, publication.PublicationID, PublicationEffectCI, string(terminal))
			if _, err := d.AuthorizePublicationEffect(AuthorizePublicationEffectInput{
				PublicationID:  publication.PublicationID,
				Kind:           PublicationEffectCI,
				Binding:        effect.Binding,
				DecisionDigest: "not-applicable",
			}); !errors.Is(err, ErrPublicationAuthorizationNotAllowed) {
				t.Fatalf("CI authorization error = %v, want ErrPublicationAuthorizationNotAllowed", err)
			}

			started, err := d.BeginPublicationEffect(BeginPublicationEffectInput{
				PublicationID: publication.PublicationID,
				Kind:          PublicationEffectCI,
				Binding:       effect.Binding,
			})
			if err != nil {
				t.Fatalf("begin read-only CI observation: %v", err)
			}
			if started.EffectStartedAt == nil || started.DecisionDigest != nil || started.DecisionConsumedAt != nil {
				t.Fatalf("CI journal gained an Owner decision or no start marker: %#v", started)
			}
			changed := effect.Binding
			changed.CandidateSHA = "3333333333333333333333333333333333333333"
			if _, err := d.ConcludePublicationEffect(ConcludePublicationEffectInput{
				PublicationID: publication.PublicationID,
				Kind:          PublicationEffectCI,
				Binding:       changed,
				State:         terminal,
			}); !errors.Is(err, ErrPublicationEffectConflict) {
				t.Fatalf("conclude with changed candidate error = %v, want ErrPublicationEffectConflict", err)
			}

			observation := []byte(`{"head":"1111111111111111111111111111111111111111","checks":["pass"]}`)
			concluded, err := d.ConcludePublicationEffect(ConcludePublicationEffectInput{
				PublicationID: publication.PublicationID,
				Kind:          PublicationEffectCI,
				Binding:       effect.Binding,
				State:         terminal,
				Observation:   observation,
			})
			if err != nil {
				t.Fatalf("conclude CI observation: %v", err)
			}
			if concluded.State != terminal || !bytes.Equal(concluded.Observation, observation) || concluded.ObservedAt == nil {
				t.Fatalf("concluded effect = %#v", concluded)
			}

			_, err = d.ConcludePublicationEffect(ConcludePublicationEffectInput{
				PublicationID: publication.PublicationID,
				Kind:          PublicationEffectCI,
				Binding:       effect.Binding,
				State:         PublicationEffectPlanned,
			})
			if !errors.Is(err, ErrPublicationEffectTransition) {
				t.Fatalf("nonterminal conclusion error = %v, want ErrPublicationEffectTransition", err)
			}
		})
	}
}

func TestPublicationRecoveryIsSeparateFromOrdinaryStaleRunFailure(t *testing.T) {
	d := openTestDB(t)
	repo := insertPublicationRepo(t, d, "repo-recovery")

	standard, err := d.InsertRun(repo.ID, "feature/standard", "standard-head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(standard.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	input := publicationInput(repo.ID, "refs/heads/feature/publication", "recovery")
	publication, publicationRun := createPublication(t, d, input)
	if err := d.UpdateRunStatus(publicationRun.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	planEffect(t, d, publication.PublicationID, PublicationEffectPush, "recovery")

	recoverable, err := d.ListRecoverablePublicationRuns()
	if err != nil {
		t.Fatalf("list recoverable publication runs: %v", err)
	}
	if len(recoverable) != 1 || recoverable[0].ID != publicationRun.ID || recoverable[0].Kind != RunKindFactoryPublicationV1 {
		t.Fatalf("recoverable publication runs = %#v, want only %s", recoverable, publicationRun.ID)
	}

	count, err := d.RecoverStaleRuns("daemon crashed")
	if err != nil {
		t.Fatalf("recover ordinary stale runs: %v", err)
	}
	if count != 1 {
		t.Fatalf("ordinary stale recovery count = %d, want 1", count)
	}
	gotStandard, err := d.GetRun(standard.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotPublication, err := d.GetRun(publicationRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotStandard.Status != types.RunFailed {
		t.Fatalf("ordinary stale run status = %s, want failed", gotStandard.Status)
	}
	if gotPublication.Status != types.RunRunning || gotPublication.Kind != RunKindFactoryPublicationV1 {
		t.Fatalf("publication run was consumed by ordinary stale recovery: %#v", gotPublication)
	}
}
