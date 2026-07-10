package db

import "testing"

// TestSealsAreAppendOnly confirms a reseal creates a new record and LatestSeal
// returns the newest, rather than rewriting the prior seal.
func TestSealsAreAppendOnly(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/seal-repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "refs/heads/feature", "aaaa", "bbbb")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	if latest, err := d.LatestSeal(run.ID); err != nil {
		t.Fatalf("latest seal (empty): %v", err)
	} else if latest != nil {
		t.Fatalf("expected no seal before any is created, got %+v", latest)
	}

	first, err := d.CreateSeal(run.ID, "sha-one", "pre_verify")
	if err != nil {
		t.Fatalf("create first seal: %v", err)
	}
	second, err := d.CreateSeal(run.ID, "sha-two", "ci_republish")
	if err != nil {
		t.Fatalf("create second seal: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("reseal must create a distinct record, not rewrite the prior one")
	}

	latest, err := d.LatestSeal(run.ID)
	if err != nil {
		t.Fatalf("latest seal: %v", err)
	}
	if latest.SHA != "sha-two" || latest.Reason != "ci_republish" {
		t.Fatalf("latest seal = %q/%q, want sha-two/ci_republish", latest.SHA, latest.Reason)
	}
}
