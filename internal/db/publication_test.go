package db

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

type publicationFixture struct {
	d    *DB
	run  *Run
	seal *Seal
}

func newPublicationFixture(t *testing.T) publicationFixture {
	t.Helper()
	d := openTestDB(t)
	return seedPublicationFixture(t, d)
}

func seedPublicationFixture(t *testing.T, d *DB) publicationFixture {
	t.Helper()
	repo, err := d.InsertRepo(t.TempDir(), "git@github.com:test/publication.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature/publication", "old-head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	seal, err := d.CreateSeal(run.ID, "sealed-head", "pre-publish")
	if err != nil {
		t.Fatalf("create seal: %v", err)
	}
	return publicationFixture{d: d, run: run, seal: seal}
}

func publicationInput(f publicationFixture, kind PublicationKind) PreparePublicationInput {
	return PreparePublicationInput{
		RunID:             f.run.ID,
		Kind:              kind,
		SealID:            f.seal.ID,
		SealSHA:           f.seal.SHA,
		DestinationURL:    "git@github.com:test/publication.git",
		DestinationRef:    "refs/heads/feature/publication",
		ExpectedRemoteSHA: "remote-before",
		Force:             true,
	}
}

func ciSealedPublicationInput(f publicationFixture, sha string) PrepareCISealedPublicationInput {
	return PrepareCISealedPublicationInput{
		RunID:             f.run.ID,
		SHA:               sha,
		DestinationURL:    "git@github.com:test/publication.git",
		DestinationRef:    "refs/heads/feature/publication",
		ExpectedRemoteSHA: "remote-before",
		Force:             true,
	}
}

func prepareAcceptedPublication(t *testing.T, f publicationFixture, kind PublicationKind) *Publication {
	t.Helper()
	publication, err := f.d.PreparePublication(publicationInput(f, kind))
	if err != nil {
		t.Fatalf("prepare publication: %v", err)
	}
	if err := f.d.MarkPublicationAttempted(publication.ID); err != nil {
		t.Fatalf("mark publication attempted: %v", err)
	}
	if err := f.d.MarkPublicationAccepted(publication.ID); err != nil {
		t.Fatalf("mark publication accepted: %v", err)
	}
	publication, err = f.d.GetPublication(publication.ID)
	if err != nil {
		t.Fatalf("get accepted publication: %v", err)
	}
	return publication
}

func preparePublicationForSHA(t *testing.T, f publicationFixture, kind PublicationKind, sha string) *Publication {
	t.Helper()
	seal, err := f.d.CreateSeal(f.run.ID, sha, "publication-recovery-test")
	if err != nil {
		t.Fatalf("create publication seal: %v", err)
	}
	input := publicationInput(f, kind)
	input.SealID = seal.ID
	input.SealSHA = seal.SHA
	publication, err := f.d.PreparePublication(input)
	if err != nil {
		t.Fatalf("prepare publication for %s: %v", sha, err)
	}
	return publication
}

func acceptPublication(t *testing.T, d *DB, publication *Publication) *Publication {
	t.Helper()
	if err := d.MarkPublicationAttempted(publication.ID); err != nil {
		t.Fatalf("mark publication attempted: %v", err)
	}
	if err := d.MarkPublicationAccepted(publication.ID); err != nil {
		t.Fatalf("mark publication accepted: %v", err)
	}
	accepted, err := d.GetPublication(publication.ID)
	if err != nil {
		t.Fatalf("get accepted publication: %v", err)
	}
	return accepted
}

func setPublicationCreatedAt(t *testing.T, d *DB, publicationID string, createdAt int64) {
	t.Helper()
	result, err := d.sql.Exec(`UPDATE publication_transactions SET created_at = ? WHERE id = ?`, createdAt, publicationID)
	if err != nil {
		t.Fatalf("set publication creation time: %v", err)
	}
	if err := requireOneChangedRow(result, "set publication creation time"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenMigratesPublicationCleanupSnapshotColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "publication-migration.sqlite")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`ALTER TABLE publication_transactions DROP COLUMN cleanup_snapshot_dir`); err != nil {
		t.Fatalf("drop cleanup snapshot column: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d, err = Open(dbPath)
	if err != nil {
		t.Fatalf("reopen migrated database: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if !hasColumn(t, d, "publication_transactions", "cleanup_snapshot_dir") {
		t.Fatal("migrated publication_transactions.cleanup_snapshot_dir is missing")
	}
}

func TestPreparePublicationIsImmutableAndIdempotent(t *testing.T) {
	f := newPublicationFixture(t)
	input := publicationInput(f, PublicationKindPush)

	first, err := f.d.PreparePublication(input)
	if err != nil {
		t.Fatalf("prepare publication: %v", err)
	}
	retried, err := f.d.PreparePublication(input)
	if err != nil {
		t.Fatalf("retry publication preparation: %v", err)
	}
	if !reflect.DeepEqual(retried, first) {
		t.Fatalf("retried publication = %+v, want original %+v", retried, first)
	}

	mismatches := []struct {
		name   string
		mutate func(*PreparePublicationInput)
	}{
		{name: "seal SHA", mutate: func(input *PreparePublicationInput) { input.SealSHA = "different-seal" }},
		{name: "destination URL", mutate: func(input *PreparePublicationInput) { input.DestinationURL = "git@github.com:test/other.git" }},
		{name: "destination ref", mutate: func(input *PreparePublicationInput) { input.DestinationRef = "refs/heads/other" }},
		{name: "expected remote SHA", mutate: func(input *PreparePublicationInput) { input.ExpectedRemoteSHA = "different-remote" }},
		{name: "force", mutate: func(input *PreparePublicationInput) { input.Force = false }},
		{name: "cleanup snapshot", mutate: func(input *PreparePublicationInput) { input.CleanupSnapshotDir = "/different/snapshot" }},
	}
	for _, tc := range mismatches {
		t.Run(tc.name, func(t *testing.T) {
			changed := input
			tc.mutate(&changed)
			if got, err := f.d.PreparePublication(changed); err == nil || got != nil {
				t.Fatalf("mismatched preparation = (%+v, %v), want closed failure", got, err)
			}
			got, err := f.d.GetPublication(first.ID)
			if err != nil {
				t.Fatalf("get original publication: %v", err)
			}
			if !reflect.DeepEqual(got, first) {
				t.Fatalf("original publication mutated: got %+v, want %+v", got, first)
			}
		})
	}
}

func TestPrepareCISealedPublicationRollsBackBothJournalRowsOnInsertFailure(t *testing.T) {
	tests := []struct {
		name    string
		trigger string
		timing  string
	}{
		{
			name:    "publication insert",
			trigger: "fail_ci_publication_insert",
			timing:  "INSERT ON publication_transactions",
		},
		{
			name:    "seal insert",
			trigger: "fail_ci_seal_insert",
			timing:  "INSERT ON run_seals WHEN NEW.reason = 'ci_republish'",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicationFixture(t)
			installFailTrigger(t, f.d, tc.trigger, tc.timing)

			seal, publication, err := f.d.PrepareCISealedPublication(ciSealedPublicationInput(f, "ci-head"))
			if err == nil || !strings.Contains(err.Error(), "injected "+tc.trigger+" failure") {
				t.Fatalf("prepare CI sealed publication error = %v, want injected failure", err)
			}
			if seal != nil || publication != nil {
				t.Fatalf("failed preparation = (%+v, %+v), want no returned rows", seal, publication)
			}
			if got := countRows(t, f.d, "run_seals"); got != 1 {
				t.Fatalf("seal rows after rollback = %d, want original row only", got)
			}
			if got := countRows(t, f.d, "publication_transactions"); got != 0 {
				t.Fatalf("publication rows after rollback = %d, want none", got)
			}
			latest, err := f.d.LatestSealByReason(f.run.ID, "ci_republish")
			if err != nil {
				t.Fatalf("latest CI republish seal: %v", err)
			}
			if latest != nil {
				t.Fatalf("CI republish seal survived failed transaction: %+v", latest)
			}
		})
	}
}

func TestPrepareCISealedPublicationIsImmutableAndIdempotentAcrossReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ci-publication.sqlite")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	f := seedPublicationFixture(t, d)
	input := ciSealedPublicationInput(f, "ci-head")
	firstSeal, firstPublication, err := d.PrepareCISealedPublication(input)
	if err != nil {
		t.Fatalf("prepare CI sealed publication: %v", err)
	}
	if firstSeal.Reason != "ci_republish" || firstSeal.SHA != input.SHA {
		t.Fatalf("prepared seal = %+v, want exact ci_republish SHA %q", firstSeal, input.SHA)
	}
	if firstPublication.Kind != PublicationKindCI ||
		firstPublication.SealID != firstSeal.ID ||
		firstPublication.SealSHA != firstSeal.SHA {
		t.Fatalf("prepared publication = %+v, want CI transaction for seal %+v", firstPublication, firstSeal)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	d, err = Open(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	retriedSeal, retriedPublication, err := d.PrepareCISealedPublication(input)
	if err != nil {
		t.Fatalf("retry CI sealed publication: %v", err)
	}
	if !reflect.DeepEqual(retriedSeal, firstSeal) || !reflect.DeepEqual(retriedPublication, firstPublication) {
		t.Fatalf(
			"retried preparation = (%+v, %+v), want original (%+v, %+v)",
			retriedSeal,
			retriedPublication,
			firstSeal,
			firstPublication,
		)
	}

	mismatches := []struct {
		name   string
		mutate func(*PrepareCISealedPublicationInput)
	}{
		{name: "destination URL", mutate: func(input *PrepareCISealedPublicationInput) {
			input.DestinationURL = "git@github.com:test/other.git"
		}},
		{name: "destination ref", mutate: func(input *PrepareCISealedPublicationInput) {
			input.DestinationRef = "refs/heads/other"
		}},
		{name: "expected remote SHA", mutate: func(input *PrepareCISealedPublicationInput) {
			input.ExpectedRemoteSHA = "different-remote"
		}},
		{name: "force", mutate: func(input *PrepareCISealedPublicationInput) {
			input.Force = false
		}},
		{name: "cleanup snapshot", mutate: func(input *PrepareCISealedPublicationInput) {
			input.CleanupSnapshotDir = "/different/snapshot"
		}},
	}
	for _, tc := range mismatches {
		t.Run(tc.name, func(t *testing.T) {
			changed := input
			tc.mutate(&changed)
			if seal, publication, err := d.PrepareCISealedPublication(changed); err == nil || seal != nil || publication != nil {
				t.Fatalf("mismatched retry = (%+v, %+v, %v), want closed failure", seal, publication, err)
			}
			got, err := d.GetPublication(firstPublication.ID)
			if err != nil {
				t.Fatalf("get original publication: %v", err)
			}
			if !reflect.DeepEqual(got, firstPublication) {
				t.Fatalf("original publication mutated: got %+v, want %+v", got, firstPublication)
			}
		})
	}
}

func TestPrepareCISealedPublicationAppendsForNewSHAAfterCompletion(t *testing.T) {
	f := newPublicationFixture(t)
	firstSeal, firstPublication, err := f.d.PrepareCISealedPublication(ciSealedPublicationInput(f, "ci-head-one"))
	if err != nil {
		t.Fatalf("prepare first CI sealed publication: %v", err)
	}
	if err := f.d.MarkPublicationAttempted(firstPublication.ID); err != nil {
		t.Fatalf("mark first publication attempted: %v", err)
	}
	if err := f.d.MarkPublicationAccepted(firstPublication.ID); err != nil {
		t.Fatalf("mark first publication accepted: %v", err)
	}
	if err := f.d.CompletePublication(firstPublication.ID, PublicationRecoveryNone); err != nil {
		t.Fatalf("complete first publication: %v", err)
	}

	secondSeal, secondPublication, err := f.d.PrepareCISealedPublication(ciSealedPublicationInput(f, "ci-head-two"))
	if err != nil {
		t.Fatalf("prepare second CI sealed publication: %v", err)
	}
	if secondSeal.ID == firstSeal.ID || secondPublication.ID == firstPublication.ID {
		t.Fatalf(
			"second preparation reused prior rows: first=(%s, %s), second=(%s, %s)",
			firstSeal.ID,
			firstPublication.ID,
			secondSeal.ID,
			secondPublication.ID,
		)
	}
	if secondSeal.SHA != "ci-head-two" || secondSeal.Reason != "ci_republish" {
		t.Fatalf("second seal = %+v, want appended ci_republish seal for ci-head-two", secondSeal)
	}
	if secondPublication.SealID != secondSeal.ID || secondPublication.SealSHA != secondSeal.SHA {
		t.Fatalf("second publication = %+v, want transaction for second seal %+v", secondPublication, secondSeal)
	}
	if got := countRows(t, f.d, "run_seals"); got != 3 {
		t.Fatalf("seal rows = %d, want pre-publish and two append-only CI seals", got)
	}
	if got := countRows(t, f.d, "publication_transactions"); got != 2 {
		t.Fatalf("publication rows = %d, want two CI transactions", got)
	}
}

func TestPrepareCISealedPublicationRejectsDifferentSHAWhilePriorIsIncomplete(t *testing.T) {
	f := newPublicationFixture(t)
	firstSeal, firstPublication, err := f.d.PrepareCISealedPublication(ciSealedPublicationInput(f, "ci-head-one"))
	if err != nil {
		t.Fatalf("prepare first CI sealed publication: %v", err)
	}

	seal, publication, err := f.d.PrepareCISealedPublication(ciSealedPublicationInput(f, "ci-head-two"))
	if err == nil || seal != nil || publication != nil {
		t.Fatalf("prepare different SHA with pending publication = (%+v, %+v, %v), want closed failure", seal, publication, err)
	}
	if got := countRows(t, f.d, "run_seals"); got != 2 {
		t.Fatalf("seal rows after rejected preparation = %d, want original and first CI seal", got)
	}
	if got := countRows(t, f.d, "publication_transactions"); got != 1 {
		t.Fatalf("publication rows after rejected preparation = %d, want first CI transaction", got)
	}
	latest, err := f.d.LatestSealByReason(f.run.ID, "ci_republish")
	if err != nil {
		t.Fatalf("latest CI seal: %v", err)
	}
	if !reflect.DeepEqual(latest, firstSeal) {
		t.Fatalf("latest CI seal after rejection = %+v, want %+v", latest, firstSeal)
	}
	got, err := f.d.GetPublication(firstPublication.ID)
	if err != nil {
		t.Fatalf("get first publication: %v", err)
	}
	if !reflect.DeepEqual(got, firstPublication) {
		t.Fatalf("first publication mutated: got %+v, want %+v", got, firstPublication)
	}
}

func TestPublicationPendingTransactionsSurviveReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "publication.sqlite")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	f := seedPublicationFixture(t, d)
	prepared, err := d.PreparePublication(publicationInput(f, PublicationKindPush))
	if err != nil {
		t.Fatalf("prepare publication: %v", err)
	}
	if err := d.MarkPublicationAttempted(prepared.ID); err != nil {
		t.Fatalf("mark attempted: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	d, err = Open(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	got, err := d.GetPublication(prepared.ID)
	if err != nil {
		t.Fatalf("get publication after reopen: %v", err)
	}
	if got == nil || got.ID != prepared.ID || got.State != PublicationStateAttempted {
		t.Fatalf("publication after reopen = %+v, want attempted %s", got, prepared.ID)
	}
	latest, err := d.LatestPublication(f.run.ID, PublicationKindPush)
	if err != nil {
		t.Fatalf("latest publication: %v", err)
	}
	if !reflect.DeepEqual(latest, got) {
		t.Fatalf("latest publication = %+v, want %+v", latest, got)
	}
	pending, err := d.PendingPublications()
	if err != nil {
		t.Fatalf("pending publications: %v", err)
	}
	if len(pending) != 1 || !reflect.DeepEqual(pending[0], got) {
		t.Fatalf("pending publications = %+v, want only %+v", pending, got)
	}
}

func TestRecoverablePublicationsIncludesIncompleteAndCompletedWithRunningStep(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "recoverable-publications.sqlite")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	preparedFixture := seedPublicationFixture(t, d)
	prepared, err := d.PreparePublication(publicationInput(preparedFixture, PublicationKindPush))
	if err != nil {
		t.Fatalf("prepare prepared publication: %v", err)
	}

	attemptedFixture := seedPublicationFixture(t, d)
	attempted, err := d.PreparePublication(publicationInput(attemptedFixture, PublicationKindPush))
	if err != nil {
		t.Fatalf("prepare attempted publication: %v", err)
	}
	if err := d.MarkPublicationAttempted(attempted.ID); err != nil {
		t.Fatalf("mark attempted publication: %v", err)
	}

	acceptedFixture := seedPublicationFixture(t, d)
	accepted := prepareAcceptedPublication(t, acceptedFixture, PublicationKindCI)

	completedPushFixture := seedPublicationFixture(t, d)
	completedPush := prepareAcceptedPublication(t, completedPushFixture, PublicationKindPush)
	completedPushStep, err := d.InsertStepResult(completedPushFixture.run.ID, types.StepPush)
	if err != nil {
		t.Fatalf("insert running push step: %v", err)
	}
	if err := d.StartStep(completedPushStep.ID); err != nil {
		t.Fatalf("start running push step: %v", err)
	}
	if err := d.CompletePublication(completedPush.ID, PublicationRecoveryNone); err != nil {
		t.Fatalf("complete push publication: %v", err)
	}

	completedCIFixture := seedPublicationFixture(t, d)
	completedCI := prepareAcceptedPublication(t, completedCIFixture, PublicationKindCI)
	completedCIStep, err := d.InsertStepResult(completedCIFixture.run.ID, types.StepCI)
	if err != nil {
		t.Fatalf("insert running CI step: %v", err)
	}
	if err := d.StartStep(completedCIStep.ID); err != nil {
		t.Fatalf("start running CI step: %v", err)
	}
	if err := d.CompletePublication(completedCI.ID, PublicationRecoveryNone); err != nil {
		t.Fatalf("complete CI publication: %v", err)
	}

	finishedPushFixture := seedPublicationFixture(t, d)
	finishedPush := prepareAcceptedPublication(t, finishedPushFixture, PublicationKindPush)
	finishedPushStep, err := d.InsertStepResult(finishedPushFixture.run.ID, types.StepPush)
	if err != nil {
		t.Fatalf("insert finished push step: %v", err)
	}
	if err := d.StartStep(finishedPushStep.ID); err != nil {
		t.Fatalf("start finished push step: %v", err)
	}
	if err := d.CompletePublication(finishedPush.ID, PublicationRecoveryNone); err != nil {
		t.Fatalf("complete finished push publication: %v", err)
	}
	if err := d.CompleteStep(finishedPushStep.ID, 0, 1, "push.log"); err != nil {
		t.Fatalf("complete push step: %v", err)
	}

	pendingCIFixture := seedPublicationFixture(t, d)
	pendingCI := prepareAcceptedPublication(t, pendingCIFixture, PublicationKindCI)
	if _, err := d.InsertStepResult(pendingCIFixture.run.ID, types.StepCI); err != nil {
		t.Fatalf("insert pending CI step: %v", err)
	}
	if err := d.CompletePublication(pendingCI.ID, PublicationRecoveryNone); err != nil {
		t.Fatalf("complete pending CI publication: %v", err)
	}

	unrelatedStepFixture := seedPublicationFixture(t, d)
	unrelatedStepPublication := prepareAcceptedPublication(t, unrelatedStepFixture, PublicationKindPush)
	unrelatedStep, err := d.InsertStepResult(unrelatedStepFixture.run.ID, types.StepCI)
	if err != nil {
		t.Fatalf("insert unrelated CI step: %v", err)
	}
	if err := d.StartStep(unrelatedStep.ID); err != nil {
		t.Fatalf("start unrelated CI step: %v", err)
	}
	if err := d.CompletePublication(unrelatedStepPublication.ID, PublicationRecoveryNone); err != nil {
		t.Fatalf("complete publication with unrelated step: %v", err)
	}

	if err := d.Close(); err != nil {
		t.Fatalf("close db before recovery: %v", err)
	}
	d, err = Open(dbPath)
	if err != nil {
		t.Fatalf("reopen db for recovery: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	recoverable, err := d.RecoverablePublications()
	if err != nil {
		t.Fatalf("recoverable publications: %v", err)
	}
	wantStates := map[string]PublicationState{
		prepared.ID:      PublicationStatePrepared,
		attempted.ID:     PublicationStateAttempted,
		accepted.ID:      PublicationStateAccepted,
		completedPush.ID: PublicationStateCompleted,
		completedCI.ID:   PublicationStateCompleted,
	}
	if len(recoverable) != len(wantStates) {
		t.Fatalf("recoverable publication count = %d, want %d: %+v", len(recoverable), len(wantStates), recoverable)
	}
	for _, publication := range recoverable {
		wantState, ok := wantStates[publication.ID]
		if !ok {
			t.Fatalf("unexpected recoverable publication: %+v", publication)
		}
		if publication.State != wantState {
			t.Fatalf("recoverable publication %s state = %q, want %q", publication.ID, publication.State, wantState)
		}
	}

	pending, err := d.PendingPublications()
	if err != nil {
		t.Fatalf("pending publications: %v", err)
	}
	wantPending := map[string]bool{
		prepared.ID:  true,
		attempted.ID: true,
		accepted.ID:  true,
	}
	if len(pending) != len(wantPending) {
		t.Fatalf("pending publication count = %d, want %d: %+v", len(pending), len(wantPending), pending)
	}
	for _, publication := range pending {
		if !wantPending[publication.ID] {
			t.Fatalf("completed publication leaked into pending results: %+v", publication)
		}
	}
}

func TestRecoverablePublicationsSelectsLatestAuthoritativePublicationPerActiveRunAndKind(t *testing.T) {
	d := openTestDB(t)

	incompleteFixture := seedPublicationFixture(t, d)
	olderCompleted := acceptPublication(t, d, preparePublicationForSHA(t, incompleteFixture, PublicationKindPush, "older-completed"))
	runningPush, err := d.InsertStepResult(incompleteFixture.run.ID, types.StepPush)
	if err != nil {
		t.Fatalf("insert running push step: %v", err)
	}
	if err := d.StartStep(runningPush.ID); err != nil {
		t.Fatalf("start running push step: %v", err)
	}
	if err := d.CompletePublication(olderCompleted.ID, PublicationRecoveryNone); err != nil {
		t.Fatalf("complete older push publication: %v", err)
	}
	newerIncomplete := preparePublicationForSHA(t, incompleteFixture, PublicationKindPush, "newer-incomplete")
	setPublicationCreatedAt(t, d, olderCompleted.ID, 100)
	setPublicationCreatedAt(t, d, newerIncomplete.ID, 200)

	completedFixture := seedPublicationFixture(t, d)
	oldestSuccessfulCI := acceptPublication(t, d, preparePublicationForSHA(t, completedFixture, PublicationKindCI, "oldest-successful-ci"))
	if err := d.CompletePublication(oldestSuccessfulCI.ID, PublicationRecoveryNone); err != nil {
		t.Fatalf("complete oldest CI publication: %v", err)
	}
	newestSuccessfulCI := acceptPublication(t, d, preparePublicationForSHA(t, completedFixture, PublicationKindCI, "newest-successful-ci"))
	if err := d.CompletePublication(newestSuccessfulCI.ID, PublicationRecoveryNone); err != nil {
		t.Fatalf("complete newest CI publication: %v", err)
	}
	runningCI, err := d.InsertStepResult(completedFixture.run.ID, types.StepCI)
	if err != nil {
		t.Fatalf("insert running CI step: %v", err)
	}
	if err := d.StartStep(runningCI.ID); err != nil {
		t.Fatalf("start running CI step: %v", err)
	}
	setPublicationCreatedAt(t, d, oldestSuccessfulCI.ID, 300)
	setPublicationCreatedAt(t, d, newestSuccessfulCI.ID, 400)

	terminalStatuses := []types.RunStatus{types.RunFailed, types.RunCancelled, types.RunCompleted}
	terminalPublicationIDs := make(map[string]bool, len(terminalStatuses))
	for _, status := range terminalStatuses {
		fixture := seedPublicationFixture(t, d)
		publication := preparePublicationForSHA(t, fixture, PublicationKindPush, "terminal-"+string(status))
		terminalPublicationIDs[publication.ID] = true
		if err := d.UpdateRunStatus(fixture.run.ID, status); err != nil {
			t.Fatalf("mark run %s: %v", status, err)
		}
	}

	recoverable, err := d.RecoverablePublications()
	if err != nil {
		t.Fatalf("recoverable publications: %v", err)
	}
	if len(recoverable) != 2 {
		t.Fatalf("recoverable publications = %+v, want latest incomplete push and latest completed CI only", recoverable)
	}
	wantByKind := map[PublicationKind]string{
		PublicationKindPush: newerIncomplete.ID,
		PublicationKindCI:   newestSuccessfulCI.ID,
	}
	seen := make(map[string]bool, len(recoverable))
	for _, publication := range recoverable {
		key := publication.RunID + ":" + string(publication.Kind)
		if seen[key] {
			t.Fatalf("multiple recoverable publications for %s: %+v", key, recoverable)
		}
		seen[key] = true
		if terminalPublicationIDs[publication.ID] {
			t.Fatalf("terminal run publication is recoverable: %+v", publication)
		}
		if wantID := wantByKind[publication.Kind]; publication.ID != wantID {
			t.Fatalf("recoverable %s publication = %s, want %s", publication.Kind, publication.ID, wantID)
		}
	}
}

func TestPublicationAttemptAndAcceptTransitionsAreMonotonicAndIdempotent(t *testing.T) {
	f := newPublicationFixture(t)
	publication, err := f.d.PreparePublication(publicationInput(f, PublicationKindPush))
	if err != nil {
		t.Fatalf("prepare publication: %v", err)
	}
	if err := f.d.MarkPublicationAccepted(publication.ID); err == nil {
		t.Fatal("accepted publication before attempted")
	}
	if err := f.d.MarkPublicationAttempted(publication.ID); err != nil {
		t.Fatalf("mark attempted: %v", err)
	}
	attempted, err := f.d.GetPublication(publication.ID)
	if err != nil {
		t.Fatalf("get attempted publication: %v", err)
	}
	if attempted.State != PublicationStateAttempted || attempted.AttemptedAt == nil {
		t.Fatalf("attempted publication = %+v", attempted)
	}
	if err := f.d.MarkPublicationAttempted(publication.ID); err != nil {
		t.Fatalf("repeat attempted transition: %v", err)
	}
	attemptedAgain, _ := f.d.GetPublication(publication.ID)
	if !reflect.DeepEqual(attemptedAgain, attempted) {
		t.Fatalf("repeat attempt changed publication: got %+v, want %+v", attemptedAgain, attempted)
	}

	if err := f.d.MarkPublicationAccepted(publication.ID); err != nil {
		t.Fatalf("mark accepted: %v", err)
	}
	accepted, err := f.d.GetPublication(publication.ID)
	if err != nil {
		t.Fatalf("get accepted publication: %v", err)
	}
	if accepted.State != PublicationStateAccepted || accepted.AcceptedAt == nil {
		t.Fatalf("accepted publication = %+v", accepted)
	}
	if err := f.d.MarkPublicationAccepted(publication.ID); err != nil {
		t.Fatalf("repeat accepted transition: %v", err)
	}
	if err := f.d.MarkPublicationAttempted(publication.ID); err != nil {
		t.Fatalf("late attempted transition: %v", err)
	}
	acceptedAgain, _ := f.d.GetPublication(publication.ID)
	if !reflect.DeepEqual(acceptedAgain, accepted) {
		t.Fatalf("monotonic retries changed publication: got %+v, want %+v", acceptedAgain, accepted)
	}
}

func TestCompletePublicationAtomicallyProjectsRunAndRecoveryStep(t *testing.T) {
	tests := []struct {
		name       string
		kind       PublicationKind
		mode       PublicationRecoveryStepMode
		stepName   types.StepName
		wantStatus types.StepStatus
	}{
		{name: "without step recovery", kind: PublicationKindPush, mode: PublicationRecoveryNone},
		{name: "complete running push", kind: PublicationKindPush, mode: PublicationRecoveryCompletePush, stepName: types.StepPush, wantStatus: types.StepStatusCompleted},
		{name: "reset running CI", kind: PublicationKindCI, mode: PublicationRecoveryResetCI, stepName: types.StepCI, wantStatus: types.StepStatusPending},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicationFixture(t)
			publication := prepareAcceptedPublication(t, f, tc.kind)
			var step *StepResult
			var rounds []*StepRound
			if tc.stepName != "" {
				var err error
				step, err = f.d.InsertStepResult(f.run.ID, tc.stepName)
				if err != nil {
					t.Fatalf("insert recovery step: %v", err)
				}
				if err := f.d.StartStep(step.ID); err != nil {
					t.Fatalf("start recovery step: %v", err)
				}
				for roundNumber := 1; roundNumber <= 2; roundNumber++ {
					round, err := f.d.ReserveStepRound(step.ID, roundNumber, "initial")
					if err != nil {
						t.Fatalf("reserve recovery round %d: %v", roundNumber, err)
					}
					if _, err := f.d.sql.Exec(`UPDATE step_rounds SET started_at = ? WHERE id = ?`, now()-2, round.ID); err != nil {
						t.Fatalf("backdate recovery round: %v", err)
					}
					rounds = append(rounds, round)
				}
			}

			if err := f.d.CompletePublication(publication.ID, tc.mode); err != nil {
				t.Fatalf("complete publication: %v", err)
			}
			completed, err := f.d.GetPublication(publication.ID)
			if err != nil {
				t.Fatalf("get completed publication: %v", err)
			}
			if completed.State != PublicationStateCompleted || completed.CompletedAt == nil {
				t.Fatalf("completed publication = %+v", completed)
			}
			run, err := f.d.GetRun(f.run.ID)
			if err != nil {
				t.Fatalf("get projected run: %v", err)
			}
			if run.HeadSHA != f.seal.SHA {
				t.Fatalf("run head = %q, want sealed SHA %q", run.HeadSHA, f.seal.SHA)
			}
			if step != nil {
				gotStep, err := f.d.GetStepResult(step.ID)
				if err != nil {
					t.Fatalf("get projected step: %v", err)
				}
				if gotStep.Status != tc.wantStatus {
					t.Fatalf("step status = %q, want %q", gotStep.Status, tc.wantStatus)
				}
			}
			for _, round := range rounds {
				gotRound, err := f.d.GetStepRound(round.ID)
				if err != nil {
					t.Fatalf("get recovered round: %v", err)
				}
				if gotRound.State != StepRoundCompleted || gotRound.CompletedAt == nil || gotRound.DurationMS < 2000 {
					t.Fatalf("recovered round = %+v, want completed with crash duration", gotRound)
				}
			}
			if step != nil {
				var reserved int
				if err := f.d.sql.QueryRow(`SELECT count(*) FROM step_rounds WHERE step_result_id = ? AND state = ?`, step.ID, StepRoundReserved).Scan(&reserved); err != nil {
					t.Fatalf("count reserved recovery rounds: %v", err)
				}
				if reserved != 0 {
					t.Fatalf("reserved recovery rounds = %d, want none", reserved)
				}
				if _, err := f.d.ReserveStepRound(step.ID, len(rounds)+1, "replay"); err != nil {
					t.Fatalf("reserve next round after recovery: %v", err)
				}
			}
			repeatErr := f.d.CompletePublication(publication.ID, tc.mode)
			if tc.mode == PublicationRecoveryNone && repeatErr != nil {
				t.Fatalf("repeat publication completion without recovery: %v", repeatErr)
			}
			if tc.mode != PublicationRecoveryNone && repeatErr == nil {
				t.Fatal("repeat recovery succeeded without a matching running step")
			}
		})
	}
}

func TestCompletePublicationRecoversStepAfterTransactionAlreadyCompleted(t *testing.T) {
	tests := []struct {
		name       string
		kind       PublicationKind
		mode       PublicationRecoveryStepMode
		stepName   types.StepName
		wantStatus types.StepStatus
	}{
		{
			name:       "complete running push",
			kind:       PublicationKindPush,
			mode:       PublicationRecoveryCompletePush,
			stepName:   types.StepPush,
			wantStatus: types.StepStatusCompleted,
		},
		{
			name:       "reset running CI",
			kind:       PublicationKindCI,
			mode:       PublicationRecoveryResetCI,
			stepName:   types.StepCI,
			wantStatus: types.StepStatusPending,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicationFixture(t)
			publication := prepareAcceptedPublication(t, f, tc.kind)
			step, err := f.d.InsertStepResult(f.run.ID, tc.stepName)
			if err != nil {
				t.Fatalf("insert recovery step: %v", err)
			}
			if err := f.d.StartStep(step.ID); err != nil {
				t.Fatalf("start recovery step: %v", err)
			}
			round, err := f.d.ReserveStepRound(step.ID, 1, "initial")
			if err != nil {
				t.Fatalf("reserve interrupted recovery round: %v", err)
			}
			if _, err := f.d.sql.Exec(`UPDATE step_rounds SET started_at = ? WHERE id = ?`, now()-2, round.ID); err != nil {
				t.Fatalf("backdate interrupted recovery round: %v", err)
			}
			if err := f.d.CompletePublication(publication.ID, PublicationRecoveryNone); err != nil {
				t.Fatalf("complete publication transaction: %v", err)
			}
			const stableRunUpdatedAt = int64(123)
			if _, err := f.d.sql.Exec(`UPDATE runs SET updated_at = ? WHERE id = ?`, stableRunUpdatedAt, f.run.ID); err != nil {
				t.Fatalf("stabilize run timestamp: %v", err)
			}

			publicationBeforeRecovery, err := f.d.GetPublication(publication.ID)
			if err != nil {
				t.Fatalf("get publication before recovery: %v", err)
			}
			runBeforeRecovery, err := f.d.GetRun(f.run.ID)
			if err != nil {
				t.Fatalf("get run before recovery: %v", err)
			}
			if err := f.d.CompletePublication(publication.ID, tc.mode); err != nil {
				t.Fatalf("recover completed publication: %v", err)
			}

			publicationAfterRecovery, err := f.d.GetPublication(publication.ID)
			if err != nil {
				t.Fatalf("get publication after recovery: %v", err)
			}
			if !reflect.DeepEqual(publicationAfterRecovery, publicationBeforeRecovery) {
				t.Fatalf("recovery rewrote completed publication: got %+v, want %+v", publicationAfterRecovery, publicationBeforeRecovery)
			}
			runAfterRecovery, err := f.d.GetRun(f.run.ID)
			if err != nil {
				t.Fatalf("get run after recovery: %v", err)
			}
			if runAfterRecovery.HeadSHA != f.seal.SHA {
				t.Fatalf("run head after recovery = %q, want sealed SHA %q", runAfterRecovery.HeadSHA, f.seal.SHA)
			}
			if runAfterRecovery.UpdatedAt != runBeforeRecovery.UpdatedAt {
				t.Fatalf("recovery rewrote unchanged run timestamp: got %d, want %d", runAfterRecovery.UpdatedAt, runBeforeRecovery.UpdatedAt)
			}
			gotStep, err := f.d.GetStepResult(step.ID)
			if err != nil {
				t.Fatalf("get recovered step: %v", err)
			}
			if gotStep.Status != tc.wantStatus {
				t.Fatalf("recovered step status = %q, want %q", gotStep.Status, tc.wantStatus)
			}
			if tc.mode == PublicationRecoveryCompletePush {
				if gotStep.ExitCode == nil || *gotStep.ExitCode != 0 {
					t.Fatalf("recovered push exit code = %v, want 0", gotStep.ExitCode)
				}
				if gotStep.DurationMS == nil || *gotStep.DurationMS != 0 {
					t.Fatalf("recovered push duration = %v, want reconstructable zero duration", gotStep.DurationMS)
				}
				if gotStep.LogPath == nil || *gotStep.LogPath != "" {
					t.Fatalf("recovered push log path = %v, want reconstructable empty path", gotStep.LogPath)
				}
			}
			gotRound, err := f.d.GetStepRound(round.ID)
			if err != nil {
				t.Fatalf("get recovered round: %v", err)
			}
			if gotRound.State != StepRoundCompleted || gotRound.CompletedAt == nil || gotRound.DurationMS < 2000 {
				t.Fatalf("recovered round = %+v, want completed with crash duration", gotRound)
			}
			if _, err := f.d.ReserveStepRound(step.ID, round.Round+1, "replay"); err != nil {
				t.Fatalf("reserve next round after completed-publication recovery: %v", err)
			}
		})
	}
}

func TestCompletePublicationRecoveryRejectsMultipleMatchingRunningSteps(t *testing.T) {
	f := newPublicationFixture(t)
	publication := prepareAcceptedPublication(t, f, PublicationKindPush)
	var steps []*StepResult
	for range 2 {
		step, err := f.d.InsertStepResult(f.run.ID, types.StepPush)
		if err != nil {
			t.Fatalf("insert running push step: %v", err)
		}
		if err := f.d.StartStep(step.ID); err != nil {
			t.Fatalf("start running push step: %v", err)
		}
		steps = append(steps, step)
	}
	if err := f.d.CompletePublication(publication.ID, PublicationRecoveryNone); err != nil {
		t.Fatalf("complete publication transaction: %v", err)
	}
	completedBeforeRecovery, err := f.d.GetPublication(publication.ID)
	if err != nil {
		t.Fatalf("get completed publication: %v", err)
	}

	err = f.d.CompletePublication(publication.ID, PublicationRecoveryCompletePush)
	if err == nil || !strings.Contains(err.Error(), "changed 2 rows, want exactly 1") {
		t.Fatalf("recovery error = %v, want strict two-row rejection", err)
	}
	completedAfterRecovery, err := f.d.GetPublication(publication.ID)
	if err != nil {
		t.Fatalf("get publication after rejected recovery: %v", err)
	}
	if !reflect.DeepEqual(completedAfterRecovery, completedBeforeRecovery) {
		t.Fatalf("rejected recovery rewrote publication: got %+v, want %+v", completedAfterRecovery, completedBeforeRecovery)
	}
	for _, step := range steps {
		gotStep, err := f.d.GetStepResult(step.ID)
		if err != nil {
			t.Fatalf("get step after rejected recovery: %v", err)
		}
		if gotStep.Status != types.StepStatusRunning {
			t.Fatalf("step %s status after rejected recovery = %q, want running", step.ID, gotStep.Status)
		}
	}
}

func TestCompletePublicationRecoveryRejectsPushWithoutStartTimestamp(t *testing.T) {
	f := newPublicationFixture(t)
	publication := prepareAcceptedPublication(t, f, PublicationKindPush)
	step, err := f.d.InsertStepResult(f.run.ID, types.StepPush)
	if err != nil {
		t.Fatalf("insert push step: %v", err)
	}
	if err := f.d.UpdateStepStatus(step.ID, types.StepStatusRunning); err != nil {
		t.Fatalf("mark push step running without starting it: %v", err)
	}
	if err := f.d.CompletePublication(publication.ID, PublicationRecoveryNone); err != nil {
		t.Fatalf("complete publication transaction: %v", err)
	}

	err = f.d.CompletePublication(publication.ID, PublicationRecoveryCompletePush)
	if err == nil || !strings.Contains(err.Error(), "changed 0 rows, want exactly 1") {
		t.Fatalf("recovery error = %v, want incomplete running-step rejection", err)
	}
	gotStep, err := f.d.GetStepResult(step.ID)
	if err != nil {
		t.Fatalf("get rejected push step: %v", err)
	}
	if gotStep.Status != types.StepStatusRunning || gotStep.StartedAt != nil {
		t.Fatalf("rejected push step = %+v, want unchanged running step without start timestamp", gotStep)
	}
}

func TestCompletePublicationRejectsIllegalStateModeAndStep(t *testing.T) {
	t.Run("publication not accepted", func(t *testing.T) {
		f := newPublicationFixture(t)
		publication, err := f.d.PreparePublication(publicationInput(f, PublicationKindPush))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.d.CompletePublication(publication.ID, PublicationRecoveryNone); err == nil {
			t.Fatal("completed prepared publication")
		}
		assertPublicationUnprojected(t, f, publication.ID)
	})

	t.Run("invalid recovery mode", func(t *testing.T) {
		f := newPublicationFixture(t)
		publication := prepareAcceptedPublication(t, f, PublicationKindPush)
		if err := f.d.CompletePublication(publication.ID, PublicationRecoveryStepMode("invalid")); err == nil {
			t.Fatal("completed publication with invalid recovery mode")
		}
		assertPublicationUnprojected(t, f, publication.ID)
	})

	t.Run("expected running step missing", func(t *testing.T) {
		f := newPublicationFixture(t)
		publication := prepareAcceptedPublication(t, f, PublicationKindPush)
		step, err := f.d.InsertStepResult(f.run.ID, types.StepPush)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.d.CompletePublication(publication.ID, PublicationRecoveryCompletePush); err == nil {
			t.Fatal("completed publication without a running push step")
		}
		assertPublicationUnprojected(t, f, publication.ID)
		gotStep, _ := f.d.GetStepResult(step.ID)
		if gotStep.Status != types.StepStatusPending {
			t.Fatalf("rejected recovery mutated step to %q", gotStep.Status)
		}
	})
}

func TestCompletePublicationRollsBackEveryProjectionOnSQLiteFailure(t *testing.T) {
	f := newPublicationFixture(t)
	publication := prepareAcceptedPublication(t, f, PublicationKindPush)
	step, err := f.d.InsertStepResult(f.run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.d.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	round, err := f.d.ReserveStepRound(step.ID, 1, "initial")
	if err != nil {
		t.Fatalf("reserve interrupted publication round: %v", err)
	}
	_, err = f.d.sql.Exec(`
		CREATE TRIGGER fail_publication_step_projection
		BEFORE UPDATE OF status ON step_results
		BEGIN
			SELECT RAISE(ABORT, 'injected publication projection failure');
		END`)
	if err != nil {
		t.Fatalf("install failure trigger: %v", err)
	}

	err = f.d.CompletePublication(publication.ID, PublicationRecoveryCompletePush)
	if err == nil || !strings.Contains(err.Error(), "injected publication projection failure") {
		t.Fatalf("complete publication error = %v, want injected failure", err)
	}
	assertPublicationUnprojected(t, f, publication.ID)
	rolledBack, err := f.d.GetPublication(publication.ID)
	if err != nil {
		t.Fatalf("get publication after rollback: %v", err)
	}
	if rolledBack.State != PublicationStateAccepted || rolledBack.CompletedAt != nil {
		t.Fatalf("publication after rollback = %+v, want accepted without completion", rolledBack)
	}
	gotStep, err := f.d.GetStepResult(step.ID)
	if err != nil {
		t.Fatalf("get step after rollback: %v", err)
	}
	if gotStep.Status != types.StepStatusRunning {
		t.Fatalf("step status after rollback = %q, want running", gotStep.Status)
	}
	gotRound, err := f.d.GetStepRound(round.ID)
	if err != nil {
		t.Fatalf("get round after rollback: %v", err)
	}
	if gotRound.State != StepRoundReserved || gotRound.CompletedAt != nil || gotRound.DurationMS != 0 {
		t.Fatalf("round after rollback = %+v, want unchanged reservation", gotRound)
	}
}

func assertPublicationUnprojected(t *testing.T, f publicationFixture, publicationID string) {
	t.Helper()
	publication, err := f.d.GetPublication(publicationID)
	if err != nil {
		t.Fatalf("get publication: %v", err)
	}
	if publication.State == PublicationStateCompleted || publication.CompletedAt != nil {
		t.Fatalf("publication projected despite failure: %+v", publication)
	}
	run, err := f.d.GetRun(f.run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.HeadSHA != "old-head" {
		t.Fatalf("run head after failure = %q, want old-head", run.HeadSHA)
	}
}
