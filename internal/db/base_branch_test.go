package db

import "testing"

func TestRepoEffectiveBaseBranchAndMetadataPreservation(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithIDAndForkAndBase(
		"repo-base", "/tmp/repo-base", "https://example.com/parent.git", "", "main", "staging",
	)
	if err != nil {
		t.Fatalf("InsertRepoWithIDAndForkAndBase: %v", err)
	}
	if got := repo.EffectiveBaseBranch(); got != "staging" {
		t.Fatalf("EffectiveBaseBranch = %q, want staging", got)
	}

	updated, err := d.UpdateRepoMetadata(repo.ID, "https://example.com/new-parent.git", "trunk")
	if err != nil {
		t.Fatalf("UpdateRepoMetadata: %v", err)
	}
	if updated.DefaultBranch != "trunk" || updated.BaseBranch != "staging" {
		t.Fatalf("updated branches = default %q base %q, want trunk/staging", updated.DefaultBranch, updated.BaseBranch)
	}
}

func TestRunBaseBranchSnapshotAndLegacyFallback(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithIDAndForkAndBase(
		"repo-run-base", "/tmp/repo-run-base", "https://example.com/parent.git", "", "main", "staging",
	)
	if err != nil {
		t.Fatalf("InsertRepoWithIDAndForkAndBase: %v", err)
	}

	run, err := d.InsertRunWithBaseBranch(repo.ID, "feature/x", "head", "old", repo.EffectiveBaseBranch())
	if err != nil {
		t.Fatalf("InsertRunWithBaseBranch: %v", err)
	}
	if got := run.EffectiveBaseBranch(repo); got != "staging" {
		t.Fatalf("run EffectiveBaseBranch = %q, want staging", got)
	}

	if _, err := d.UpdateRepoMetadataAndBase(repo.ID, repo.UpstreamURL, "main", "release/v2"); err != nil {
		t.Fatalf("UpdateRepoMetadataAndBase: %v", err)
	}
	changedRepo, err := d.GetRepo(repo.ID)
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	persistedRun, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got := persistedRun.EffectiveBaseBranch(changedRepo); got != "staging" {
		t.Fatalf("frozen run base = %q, want staging", got)
	}

	legacy, err := d.InsertRun(repo.ID, "feature/legacy", "head2", "old2")
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	if got := legacy.EffectiveBaseBranch(changedRepo); got != changedRepo.DefaultBranch {
		t.Fatalf("legacy run base = %q, want historical default fallback %q", got, changedRepo.DefaultBranch)
	}
}
