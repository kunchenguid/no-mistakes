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
	oldSHA := "1111111111111111111111111111111111111111"
	newSHA := "2222222222222222222222222222222222222222"
	first, err := d.ReserveReceive("repo", "/gate/repo.git", "feature", "refs/heads/feature", oldSHA, newSHA, []types.StepName{types.StepReview}, "intent")
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.ReserveReceive("repo", "/gate/repo.git", "feature", "refs/heads/feature", oldSHA, newSHA, nil, "different")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.State != ReceiveReservationReserved {
		t.Fatalf("duplicate reservation = %#v, want original pending reservation", second)
	}
	if _, err := d.ReserveReceive("repo", "/gate/repo.git", "feature", "refs/heads/feature", oldSHA, "3333333333333333333333333333333333333333", nil, ""); !errors.Is(err, ErrReceiveReservationConflict) {
		t.Fatalf("conflicting reservation error = %v, want ErrReceiveReservationConflict", err)
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
	third, err := d.ReserveReceive("repo", "/gate/repo.git", "feature", "refs/heads/feature", oldSHA, newSHA, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if third.ID == first.ID {
		t.Fatal("a later identical ref transition reused the completed receive identity")
	}
}

func TestReceiveReservationRetiresOnlyWhenPending(t *testing.T) {
	d := openTestDB(t)
	if _, err := d.InsertRepoWithID("repo", "/work/repo", "https://example.com/repo.git", "main"); err != nil {
		t.Fatal(err)
	}
	reservation, err := d.ReserveReceive("repo", "/gate/repo.git", "feature", "refs/heads/feature", "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222", nil, "")
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
	reservation, err := d.ReserveReceive("repo", "/gate/repo.git", "main", "refs/heads/main", "1111111111111111111111111111111111111111", "0000000000000000000000000000000000000000", nil, "")
	if err != nil {
		t.Fatal(err)
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
