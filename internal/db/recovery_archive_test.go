package db

import (
	"strings"
	"testing"
)

func TestRecordRecoveryArchiveIsAppendOnlyAndExactRetryIsIdempotent(t *testing.T) {
	database := openTestDB(t)
	repo, err := database.InsertRepoWithID("repo-1", "/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "required", "base")
	if err != nil {
		t.Fatal(err)
	}
	record := RecoveryArchive{
		OwnerRunID: run.ID, RepoID: repo.ID, RunID: run.ID, Branch: run.Branch,
		RequiredHeadSHA: "required", PreservedHeadSHA: "preserved", ArchiveRef: "refs/heads/archive/run-1",
	}

	first, err := database.RecordRecoveryArchive(record)
	if err != nil {
		t.Fatal(err)
	}
	second, err := database.RecordRecoveryArchive(record)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || second.ID != first.ID || second.CreatedAt != first.CreatedAt {
		t.Fatalf("idempotent archive records = first %#v, second %#v", first, second)
	}

	conflict := record
	conflict.PreservedHeadSHA = "other"
	if _, err := database.RecordRecoveryArchive(conflict); err == nil || !strings.Contains(err.Error(), "different provenance") {
		t.Fatalf("conflicting archive binding error = %v", err)
	}
	records, err := database.GetRecoveryArchivesByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].PreservedHeadSHA != "preserved" {
		t.Fatalf("archive records = %#v", records)
	}
}

func TestRecoveryArchivesPermitMultipleRefsSoClassificationCanRefuseAmbiguity(t *testing.T) {
	database := openTestDB(t)
	repo, err := database.InsertRepoWithID("repo-1", "/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "required", "base")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"refs/heads/archive/one", "refs/heads/archive/two"} {
		if _, err := database.RecordRecoveryArchive(RecoveryArchive{
			OwnerRunID: run.ID, RepoID: repo.ID, RunID: run.ID, Branch: run.Branch,
			RequiredHeadSHA: "required", PreservedHeadSHA: "preserved", ArchiveRef: ref,
		}); err != nil {
			t.Fatal(err)
		}
	}
	records, err := database.GetRecoveryArchivesByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("archive records = %d, want 2", len(records))
	}
}
