package db

import (
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReceiveReservationLifecycle(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.InsertRepoWithID("repo", "/work/repo", "https://example.com/repo.git", "main"); err != nil {
		t.Fatal(err)
	}
	if err := d.RegisterReceiveSession("repo", "/gate/repo.git", "session-lifecycle", "cap-lifecycle"); err != nil {
		t.Fatal(err)
	}
	oldSHA := "1111111111111111111111111111111111111111"
	newSHA := "2222222222222222222222222222222222222222"
	first, err := d.ReserveReceiveForSession("repo", "/gate/repo.git", "feature", "refs/heads/feature", oldSHA, newSHA, "session-lifecycle", "cap-lifecycle", []types.StepName{types.StepReview}, "intent")
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.ReserveReceiveForSession("repo", "/gate/repo.git", "feature", "refs/heads/feature", oldSHA, newSHA, "session-lifecycle", "cap-lifecycle", nil, "different")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.State != ReceiveReservationReserved {
		t.Fatalf("duplicate reservation = %#v, want original pending reservation", second)
	}
	if matches, err := d.VerifyReceiveSession("repo", "/gate/repo.git", "session-lifecycle", "cap-lifecycle"); err != nil || !matches {
		t.Fatalf("issued capability match = %v, %v", matches, err)
	}
	if matches, err := d.VerifyReceiveSession("other-repo", "/gate/repo.git", "session-lifecycle", "cap-lifecycle"); err != nil || matches {
		t.Fatalf("cross-repository capability match = %v, %v", matches, err)
	}
	if matches, err := d.VerifyReceiveSession("repo", "/gate/repo.git", "session-lifecycle", "wrong-capability"); err != nil || matches {
		t.Fatalf("forged capability match = %v, %v", matches, err)
	}
	if _, err := d.ReserveReceiveForSession("repo", "/gate/repo.git", "feature", "refs/heads/feature", oldSHA, "3333333333333333333333333333333333333333", "session-other", "cap-other", nil, ""); !errors.Is(err, ErrReceiveReservationConflict) {
		t.Fatalf("conflicting reservation error = %v, want ErrReceiveReservationConflict", err)
	}
	if err := d.MarkReceivePrepared("repo", "feature", "refs/heads/feature", oldSHA, newSHA); err != nil {
		t.Fatalf("mark prepared: %v", err)
	}
	if err := d.MarkReceiveCommitted("repo", "feature", "refs/heads/feature", oldSHA, newSHA); err != nil {
		t.Fatalf("mark committed: %v", err)
	}
	if err := d.CompleteReceiveReservation(first.ID, "run-1"); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetReceiveReservation(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ReceiveReservationPublished || got.RunID == nil || *got.RunID != "run-1" {
		t.Fatalf("completed reservation = %#v", got)
	}
	pending, err := d.GetPendingReceiveReservations()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending reservations after completion = %d", len(pending))
	}
	if err := d.RetireReceiveSession("session-lifecycle"); err != nil {
		t.Fatal(err)
	}
	if matches, err := d.VerifyReceiveSession("repo", "/gate/repo.git", "session-lifecycle", "cap-lifecycle"); err != nil || matches {
		t.Fatalf("completed capability match = %v, %v", matches, err)
	}
	third, err := d.ReserveReceiveForSession("repo", "/gate/repo.git", "feature", "refs/heads/feature", oldSHA, newSHA, "session-later", "cap-later", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if third.ID == first.ID {
		t.Fatal("a later identical ref transition reused the completed receive identity")
	}
}

func TestReceiveReservationBindsIdenticalTransitionsToSession(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.InsertRepoWithID("repo", "/work/repo", "https://example.com/repo.git", "main"); err != nil {
		t.Fatal(err)
	}
	oldSHA := "1111111111111111111111111111111111111111"
	newSHA := "2222222222222222222222222222222222222222"
	first, err := d.ReserveReceiveForSession("repo", "/gate/repo.git", "main", "refs/heads/main", oldSHA, newSHA, "session-a", "cap-a", nil, "first")
	if err != nil {
		t.Fatal(err)
	}
	retry, err := d.ReserveReceiveForSession("repo", "/gate/repo.git", "main", "refs/heads/main", oldSHA, newSHA, "session-a", "cap-a", nil, "retry")
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID != first.ID {
		t.Fatalf("same-session retry created %q, want %q", retry.ID, first.ID)
	}
	if _, err := d.ReserveReceiveForSession("repo", "/gate/repo.git", "main", "refs/heads/main", oldSHA, newSHA, "session-b", "cap-b", nil, "second"); !errors.Is(err, ErrReceiveReservationConflict) {
		t.Fatalf("different identical receive error = %v, want conflict", err)
	}
}

func TestAuthenticatedReceivePhasesRequireExactCapability(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.InsertRepoWithID("repo", "/work/repo", "https://example.com/repo.git", "main"); err != nil {
		t.Fatal(err)
	}
	gate := "/gate/repo.git"
	oldSHA := "1111111111111111111111111111111111111111"
	newSHA := "2222222222222222222222222222222222222222"
	if err := d.RegisterReceiveSession("repo", gate, "session", "capability"); err != nil {
		t.Fatal(err)
	}
	reservation, err := d.ReserveReceiveForAuthenticatedSession("repo", gate, "main", "refs/heads/main", oldSHA, newSHA, "session", "capability", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MarkReceivePreparedForSession(reservation.ID, "repo", "main", "refs/heads/main", oldSHA, newSHA, "session", "wrong"); err == nil {
		t.Fatal("wrong capability prepared the receive")
	}
	if err := d.MarkReceivePreparedForSession(reservation.ID, "repo", "main", "refs/heads/main", oldSHA, newSHA, "session", "capability"); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkReceiveCommittedForSession(reservation.ID, "repo", "main", "refs/heads/main", oldSHA, newSHA, "other-session", "capability"); err == nil {
		t.Fatal("wrong session committed the receive")
	}
	if err := d.MarkReceiveCommittedForSession(reservation.ID, "repo", "main", "refs/heads/main", oldSHA, newSHA, "session", "capability"); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticatedReceiveBatchIsAtomicAndSessionScoped(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.InsertRepoWithID("repo", "/work/repo", "https://example.com/repo.git", "main"); err != nil {
		t.Fatal(err)
	}
	gate := "/gate/repo.git"
	if err := d.RegisterReceiveSession("repo", gate, "session-batch", "cap-batch"); err != nil {
		t.Fatal(err)
	}
	oldSHA := "1111111111111111111111111111111111111111"
	mainSHA := "2222222222222222222222222222222222222222"
	featureSHA := "3333333333333333333333333333333333333333"
	reservations, err := d.ReserveReceivesForAuthenticatedSession("session-batch", "cap-batch", []ReceiveReservationInput{
		{RepoID: "repo", GatePath: gate, Branch: "main", Ref: "refs/heads/main", OldSHA: oldSHA, NewSHA: mainSHA},
		{RepoID: "repo", GatePath: gate, Branch: "feature", Ref: "refs/heads/feature", OldSHA: oldSHA, NewSHA: featureSHA},
	})
	if err != nil || len(reservations) != 2 {
		t.Fatalf("reserve receive batch = %v, %v", reservations, err)
	}
	retry, err := d.ReserveReceivesForAuthenticatedSession("session-batch", "cap-batch", []ReceiveReservationInput{
		{RepoID: "repo", GatePath: gate, Branch: "feature", Ref: "refs/heads/feature", OldSHA: oldSHA, NewSHA: featureSHA},
		{RepoID: "repo", GatePath: gate, Branch: "main", Ref: "refs/heads/main", OldSHA: oldSHA, NewSHA: mainSHA},
	})
	if err != nil || len(retry) != 2 || retry[0].ID != reservations[1].ID || retry[1].ID != reservations[0].ID {
		t.Fatalf("same-batch retry = %v, %v", retry, err)
	}
	if err := d.RetireReceiveSession("session-batch"); err == nil {
		t.Fatal("retired a session with pending reservations")
	}
	if _, err := d.ReserveReceivesForAuthenticatedSession("session-batch", "cap-batch", []ReceiveReservationInput{
		{RepoID: "repo", GatePath: gate, Branch: "other", Ref: "refs/heads/other", OldSHA: oldSHA, NewSHA: mainSHA},
		{RepoID: "repo", GatePath: gate, Branch: "main", Ref: "refs/heads/main", OldSHA: oldSHA, NewSHA: featureSHA},
	}); err == nil {
		t.Fatal("accepted a conflicting batch")
	}
	other, err := d.GetPendingReceiveReservation("repo", "other", "refs/heads/other", oldSHA, mainSHA)
	if err != nil {
		t.Fatal(err)
	}
	if other != nil {
		t.Fatal("conflicting batch partially inserted a reservation")
	}
	if err := d.ApplyReceiveTransactionBatch("prepared", "session-batch", "cap-batch", []ReceiveTransactionInput{
		{ID: reservations[0].ID, RepoID: "repo", Branch: "main", Ref: "refs/heads/main", OldSHA: oldSHA, NewSHA: mainSHA},
		{ID: reservations[1].ID, RepoID: "repo", Branch: "feature", Ref: "refs/heads/feature", OldSHA: oldSHA, NewSHA: featureSHA},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyReceiveTransactionBatch("committed", "session-batch", "cap-batch", []ReceiveTransactionInput{
		{ID: reservations[0].ID, RepoID: "repo", Branch: "main", Ref: "refs/heads/main", OldSHA: oldSHA, NewSHA: mainSHA},
		{ID: reservations[1].ID, RepoID: "repo", Branch: "feature", Ref: "refs/heads/feature", OldSHA: oldSHA, NewSHA: featureSHA},
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyReceiveTransactionBatch("prepared", "session-batch", "cap-batch", []ReceiveTransactionInput{{ID: reservations[0].ID, RepoID: "repo", Branch: "main", Ref: "refs/heads/main", OldSHA: oldSHA, NewSHA: mainSHA}}); err == nil {
		t.Fatal("replayed prepared phase after commit")
	}
	if err := d.CompleteReceiveReservationForSession(reservations[0].ID, "run-main", "session-batch", "cap-batch"); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteReceiveReservationForSession(reservations[1].ID, "run-feature", "session-batch", "cap-batch"); err != nil {
		t.Fatal(err)
	}
	if err := d.RetireReceiveSession("session-batch"); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyReceiveTransactionBatch("committed", "session-batch", "cap-batch", []ReceiveTransactionInput{{ID: reservations[0].ID, RepoID: "repo", Branch: "main", Ref: "refs/heads/main", OldSHA: oldSHA, NewSHA: mainSHA}}); err == nil {
		t.Fatal("replayed committed phase after session retirement")
	}
}

func TestAbortReceiveSessionRetiresTheWholeSealedBatch(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.InsertRepoWithID("repo", "/work/repo", "https://example.com/repo.git", "main"); err != nil {
		t.Fatal(err)
	}
	gate := "/gate/repo.git"
	oldSHA := "1111111111111111111111111111111111111111"
	if err := d.RegisterReceiveSession("repo", gate, "session-abort", "cap-abort"); err != nil {
		t.Fatal(err)
	}
	reservations, err := d.ReserveReceivesForAuthenticatedSession("session-abort", "cap-abort", []ReceiveReservationInput{
		{RepoID: "repo", GatePath: gate, Branch: "main", Ref: "refs/heads/main", OldSHA: oldSHA, NewSHA: "2222222222222222222222222222222222222222"},
		{RepoID: "repo", GatePath: gate, Branch: "feature", Ref: "refs/heads/feature", OldSHA: oldSHA, NewSHA: "3333333333333333333333333333333333333333"},
	})
	if err != nil || len(reservations) != 2 {
		t.Fatalf("reserve abort batch = %v, %v", reservations, err)
	}
	if err := d.AbortReceiveSession("repo", gate, "session-abort", "cap-abort"); err != nil {
		t.Fatal(err)
	}
	for _, reservation := range reservations {
		got, err := d.GetReceiveReservation(reservation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != ReceiveReservationRetired {
			t.Fatalf("reservation %s state = %q, want retired", reservation.ID, got.State)
		}
	}
	if err := d.RetireReceiveSession("session-abort"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReserveReceivesForAuthenticatedSession("session-abort", "cap-abort", []ReceiveReservationInput{{
		RepoID: "repo", GatePath: gate, Branch: "main", Ref: "refs/heads/main", OldSHA: oldSHA, NewSHA: "4444444444444444444444444444444444444444",
	}}); err == nil {
		t.Fatal("reused an aborted and retired receive capability")
	}
}

func TestReceiveReservationRetiresOnlyWhenPending(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.InsertRepoWithID("repo", "/work/repo", "https://example.com/repo.git", "main"); err != nil {
		t.Fatal(err)
	}
	reservation, err := d.ReserveReceiveForSession("repo", "/gate/repo.git", "feature", "refs/heads/feature", "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222", "session-retire", "cap-retire", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RetireReceiveReservation(reservation.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.RetireReceiveReservation(reservation.ID); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetReceiveReservation(reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ReceiveReservationRetired {
		t.Fatalf("retired reservation state = %q", got.State)
	}
}

func TestReceiveReservationSupportsDeletion(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.InsertRepoWithID("repo", "/work/repo", "https://example.com/repo.git", "main"); err != nil {
		t.Fatal(err)
	}
	reservation, err := d.ReserveReceiveForSession("repo", "/gate/repo.git", "main", "refs/heads/main", "1111111111111111111111111111111111111111", "0000000000000000000000000000000000000000", "session-delete", "cap-delete", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MarkReceivePrepared("repo", "main", "refs/heads/main", "1111111111111111111111111111111111111111", "0000000000000000000000000000000000000000"); err != nil {
		t.Fatalf("mark deletion prepared: %v", err)
	}
	if err := d.MarkReceiveCommitted("repo", "main", "refs/heads/main", "1111111111111111111111111111111111111111", "0000000000000000000000000000000000000000"); err != nil {
		t.Fatalf("mark deletion committed: %v", err)
	}
	if err := d.CompleteReceiveReservation(reservation.ID, ""); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetReceiveReservation(reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ReceiveReservationPublished || got.RunID != nil {
		t.Fatalf("published deletion reservation = %#v", got)
	}
}

func TestReceiveReservationAbortedEvidenceRetiresPendingState(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.InsertRepoWithID("repo", "/work/repo", "https://example.com/repo.git", "main"); err != nil {
		t.Fatal(err)
	}
	oldSHA := "1111111111111111111111111111111111111111"
	newSHA := "2222222222222222222222222222222222222222"
	reservation, err := d.ReserveReceiveForSession("repo", "/gate/repo.git", "main", "refs/heads/main", oldSHA, newSHA, "session-abort", "cap-abort", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.MarkReceivePrepared("repo", "main", "refs/heads/main", oldSHA, newSHA); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkReceiveAborted("repo", "main", "refs/heads/main", oldSHA, newSHA); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetReceiveReservation(reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ReceiveReservationRetired {
		t.Fatalf("aborted reservation state = %q, want retired", got.State)
	}
}
