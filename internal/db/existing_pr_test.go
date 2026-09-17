package db

import (
	"path/filepath"
	"testing"
)

func TestExistingPRPinSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo("/test/repo", "https://github.com/contributor/widgets.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	target := "https://github.com/upstream/widgets/pull/168"
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "", "", "", "", target)
	if err != nil {
		t.Fatal(err)
	}
	if run.ExistingPRURL == nil || *run.ExistingPRURL != target || run.PRURL == nil || *run.PRURL != target {
		t.Fatalf("creation did not atomically pin target: %+v", run)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExistingPRURL == nil || *got.ExistingPRURL != target || got.PRURL == nil || *got.PRURL != target {
		t.Fatalf("reopen lost pin: %+v", got)
	}
}

// The run's pin and the branch's association are one fact; a caller that
// records the pin must not be able to observe a branch that still maps
// somewhere else.
func TestExplicitTargetPinsTheRunAndAssociatesTheBranchInOneWrite(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/test/repo", "https://github.com/contributor/widgets.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	target := "https://github.com/upstream/widgets/pull/168"
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "", "", "", "", target)
	if err != nil {
		t.Fatal(err)
	}
	if run.ExistingPRURL == nil || *run.ExistingPRURL != target {
		t.Fatalf("run not pinned to %s: %+v", target, run)
	}
	stored, err := d.GetBranchPRTarget(repo.ID, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if stored != target {
		t.Fatalf("branch association after insert = %q, want %q", stored, target)
	}
}

// A replacement that does not commit must leave the previous association
// intact: the alternative is a branch mapped to a pull request no run is
// pinned to, which the next unflagged launch would publish to.
func TestFailedExplicitRunInsertLeavesThePriorAssociationIntact(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/test/repo", "https://github.com/contributor/widgets.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	original := "https://github.com/upstream/widgets/pull/168"
	if _, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "nonce-1", "gen", "digest", "", original); err != nil {
		t.Fatal(err)
	}
	// Same repository, branch and launch nonce: the partial unique index
	// refuses this insert, so nothing it carries may land.
	replacement := "https://github.com/upstream/widgets/pull/999"
	if _, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head2", "base", nil, "nonce-1", "gen", "digest", "", replacement); err == nil {
		t.Fatal("expected duplicate launch nonce insert to fail")
	}
	stored, err := d.GetBranchPRTarget(repo.ID, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if stored != original {
		t.Fatalf("branch association = %q, want the original %q", stored, original)
	}
}
