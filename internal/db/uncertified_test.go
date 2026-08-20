package db

import "testing"

func TestUncertifiedPipelineRangeUpsertGetDelete(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/work/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head-1", "base")
	if err != nil {
		t.Fatal(err)
	}

	got, err := d.GetUncertifiedPipelineRange(repo.ID, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("empty lookup = %#v, want nil", got)
	}

	if err := d.UpsertUncertifiedPipelineRange(repo.ID, "feature", "from-a", "to-a", run.ID); err != nil {
		t.Fatal(err)
	}
	got, err = d.GetUncertifiedPipelineRange(repo.ID, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.FromSHA != "from-a" || got.ToSHA != "to-a" || got.SourceRunID != run.ID {
		t.Fatalf("first upsert = %#v", got)
	}

	run2, err := d.InsertRun(repo.ID, "feature", "head-2", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpsertUncertifiedPipelineRange(repo.ID, "feature", "from-b", "to-b", run2.ID); err != nil {
		t.Fatal(err)
	}
	got, err = d.GetUncertifiedPipelineRange(repo.ID, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.FromSHA != "from-b" || got.ToSHA != "to-b" || got.SourceRunID != run2.ID {
		t.Fatalf("replacement upsert = %#v, want latest range only", got)
	}

	other, err := d.GetUncertifiedPipelineRange(repo.ID, "other")
	if err != nil {
		t.Fatal(err)
	}
	if other != nil {
		t.Fatalf("other branch = %#v, want nil", other)
	}

	if err := d.DeleteUncertifiedPipelineRange(repo.ID, "feature"); err != nil {
		t.Fatal(err)
	}
	got, err = d.GetUncertifiedPipelineRange(repo.ID, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("after delete = %#v, want nil", got)
	}
}

func TestOpenCreatesUncertifiedPipelineRangesTable(t *testing.T) {
	d := openTestDB(t)
	var count int
	if err := d.sql.QueryRow("SELECT count(*) FROM uncertified_pipeline_ranges").Scan(&count); err != nil {
		t.Fatalf("uncertified_pipeline_ranges table missing: %v", err)
	}
}
