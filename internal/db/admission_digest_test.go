package db

import "testing"

func TestInsertRunWithAdmissionDigestPersistsIdentity(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "https://github.com/user/project.git", "main")

	run, err := d.InsertRunWithAdmissionDigest(
		repo.ID, "feature", "abc123", "def456", nil,
		"", "", "", "admission-identity", "", false, nil,
	)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.AdmissionDigest == nil || *got.AdmissionDigest != "admission-identity" {
		t.Fatalf("admission digest = %#v", got.AdmissionDigest)
	}
}
