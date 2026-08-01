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
