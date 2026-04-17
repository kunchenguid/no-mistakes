package db

import (
	"testing"
)

func TestRepoInsertAndGet(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if repo.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if repo.WorkingPath != "/home/user/project" {
		t.Errorf("working path = %q, want %q", repo.WorkingPath, "/home/user/project")
	}
	if repo.UpstreamURL != "git@github.com:user/project.git" {
		t.Errorf("upstream url = %q, want %q", repo.UpstreamURL, "git@github.com:user/project.git")
	}
	if repo.DefaultBranch != "main" {
		t.Errorf("default branch = %q, want %q", repo.DefaultBranch, "main")
	}
	if repo.CreatedAt == 0 {
		t.Error("expected non-zero created_at")
	}

	got, err := d.GetRepo(repo.ID)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil repo")
	}
	if got.ID != repo.ID {
		t.Errorf("id = %q, want %q", got.ID, repo.ID)
	}
}

func TestInsertRepoWithID(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithID("custom-id-123", "/home/user/myproject", "git@github.com:user/myproject.git", "develop")
	if err != nil {
		t.Fatalf("insert repo with id: %v", err)
	}
	if repo.ID != "custom-id-123" {
		t.Errorf("id = %q, want %q", repo.ID, "custom-id-123")
	}
	if repo.WorkingPath != "/home/user/myproject" {
		t.Errorf("working path = %q, want %q", repo.WorkingPath, "/home/user/myproject")
	}
	if repo.UpstreamURL != "git@github.com:user/myproject.git" {
		t.Errorf("upstream url = %q, want %q", repo.UpstreamURL, "git@github.com:user/myproject.git")
	}
	if repo.DefaultBranch != "develop" {
		t.Errorf("default branch = %q, want %q", repo.DefaultBranch, "develop")
	}
	if repo.CreatedAt == 0 {
		t.Error("expected non-zero created_at")
	}

	// Verify round-trip through GetRepo.
	got, err := d.GetRepo("custom-id-123")
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got == nil || got.ID != "custom-id-123" {
		t.Fatal("expected repo with custom ID")
	}
	if got.DefaultBranch != "develop" {
		t.Errorf("default branch after get = %q, want %q", got.DefaultBranch, "develop")
	}
}

func TestInsertRepoWithIDDuplicate(t *testing.T) {
	d := openTestDB(t)
	_, err := d.InsertRepoWithID("dup-id", "/path/a", "git@github.com:a/b.git", "main")
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Same ID should fail (primary key constraint).
	_, err = d.InsertRepoWithID("dup-id", "/path/b", "git@github.com:c/d.git", "main")
	if err == nil {
		t.Fatal("expected error for duplicate ID")
	}
}

func TestRepoGetByPath(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")

	got, err := d.GetRepoByPath("/home/user/project")
	if err != nil {
		t.Fatalf("get repo by path: %v", err)
	}
	if got == nil || got.ID != repo.ID {
		t.Fatalf("expected repo with ID %q", repo.ID)
	}

	got, err = d.GetRepoByPath("/nonexistent")
	if err != nil {
		t.Fatalf("get repo by path (not found): %v", err)
	}
	if got != nil {
		t.Fatal("expected nil for nonexistent path")
	}
}

func TestRepoGetNotFound(t *testing.T) {
	d := openTestDB(t)
	got, err := d.GetRepo("nonexistent")
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got != nil {
		t.Fatal("expected nil for nonexistent repo")
	}
}

func TestRepoUniqueWorkingPath(t *testing.T) {
	d := openTestDB(t)
	_, err := d.InsertRepo("/home/user/project", "git@github.com:a/b.git", "main")
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err = d.InsertRepo("/home/user/project", "git@github.com:c/d.git", "main")
	if err == nil {
		t.Fatal("expected error for duplicate working_path")
	}
}

func TestRepoDelete(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")

	if err := d.DeleteRepo(repo.ID); err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	got, _ := d.GetRepo(repo.ID)
	if got != nil {
		t.Fatal("expected nil after delete")
	}
}
