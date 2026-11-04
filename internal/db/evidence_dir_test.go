package db

import "testing"

func TestSetRunEvidenceDirDoesNotMoveAnExistingRun(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/test/repo", "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunEvidenceDir(run.ID, "/evidence/run-1"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunEvidenceDir(run.ID, "/elsewhere/run-1"); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.EvidencePath() != "/evidence/run-1" {
		t.Fatalf("evidence dir = %q, want the first pin", got.EvidencePath())
	}
}

func TestGitlabEvidenceUploadReusesTheFirstMarkdown(t *testing.T) {
	d := openTestDB(t)
	if err := d.PutGitlabEvidenceUpload("group/project", "sha256:abc", "![shot](/uploads/abc/shot.png)"); err != nil {
		t.Fatal(err)
	}
	if err := d.PutGitlabEvidenceUpload("group/project", "sha256:abc", "![shot](/uploads/other/shot.png)"); err != nil {
		t.Fatal(err)
	}
	got, found, err := d.GetGitlabEvidenceUpload("group/project", "sha256:abc")
	if err != nil || !found {
		t.Fatalf("lookup = %q found %v err %v", got, found, err)
	}
	if got != "![shot](/uploads/abc/shot.png)" {
		t.Fatalf("markdown = %q, want the first upload", got)
	}
	if _, found, err := d.GetGitlabEvidenceUpload("other/project", "sha256:abc"); err != nil || found {
		t.Fatalf("other project found=%v err=%v, want a miss", found, err)
	}
}
