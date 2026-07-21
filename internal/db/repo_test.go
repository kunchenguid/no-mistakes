package db

import (
	"bytes"
	"database/sql"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/repoexec"
)

func TestDecodeRepoGitHubContextRejectsTrailingJSONValue(t *testing.T) {
	repo := &Repo{}
	encoded := sql.NullString{Valid: true, String: `{
  "version": 1,
  "gh_path": "/usr/bin/gh",
  "git_path": "/usr/bin/git",
  "gh_config_dir": "/tmp/gh-a",
  "host": "github.com",
  "expected_login": "account-a",
  "git_protocol": "https",
  "credential_helper": "gh",
  "commit_author": {"name": "A", "email": "a@example.test"}
} {"token":"credential-sentinel"}`}
	err := decodeRepoGitHubContext(repo, encoded)
	if err == nil {
		t.Fatal("expected trailing persisted JSON value to be rejected")
	}
	if !errors.Is(err, repoexec.ErrInvalidGitHubContextJSON) {
		t.Fatalf("error = %v, want invalid context JSON", err)
	}
	if strings.Contains(err.Error(), "credential-sentinel") {
		t.Fatalf("error leaked trailing credential value: %v", err)
	}
}

func TestDecodeRepoGitHubContextDoesNotExposeUnknownFieldInLogs(t *testing.T) {
	repo := &Repo{}
	encoded := sql.NullString{Valid: true, String: `{
  "version": 1,
  "gh_path": "/usr/bin/gh",
  "git_path": "/usr/bin/git",
  "gh_config_dir": "/tmp/gh-a",
  "host": "github.com",
  "expected_login": "account-a",
  "git_protocol": "https",
  "credential_helper": "gh",
  "commit_author": {"name": "A", "email": "a@example.test"},
  "credential-sentinel": true
}`}
	err := decodeRepoGitHubContext(repo, encoded)
	if !errors.Is(err, repoexec.ErrInvalidGitHubContextJSON) {
		t.Fatalf("error = %v, want invalid context JSON", err)
	}
	var logged bytes.Buffer
	log.New(&logged, "", 0).Printf("database load failed: %v", err)
	if strings.Contains(logged.String(), "credential-sentinel") {
		t.Fatalf("log leaked credential value: %s", logged.String())
	}
}

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
	if repo.ForkURL != "" {
		t.Errorf("fork url = %q, want empty", repo.ForkURL)
	}
	if repo.PushURL() != repo.UpstreamURL {
		t.Errorf("push url = %q, want upstream %q", repo.PushURL(), repo.UpstreamURL)
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
	if got.ForkURL != "" {
		t.Errorf("fork url after get = %q, want empty", got.ForkURL)
	}
}

func TestRepoForkURLRoundTrip(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithFork("/home/user/project", "git@github.com:parent/project.git", "git@github.com:fork/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo with fork: %v", err)
	}
	if repo.ForkURL != "git@github.com:fork/project.git" {
		t.Fatalf("fork url = %q, want fork URL", repo.ForkURL)
	}
	if repo.PushURL() != repo.ForkURL {
		t.Fatalf("push url = %q, want fork URL %q", repo.PushURL(), repo.ForkURL)
	}

	got, err := d.GetRepo(repo.ID)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got == nil {
		t.Fatal("expected repo")
	}
	if got.UpstreamURL != "git@github.com:parent/project.git" {
		t.Fatalf("upstream url = %q, want parent URL", got.UpstreamURL)
	}
	if got.ForkURL != "git@github.com:fork/project.git" {
		t.Fatalf("fork url after get = %q, want fork URL", got.ForkURL)
	}
	if got.PushURL() != "git@github.com:fork/project.git" {
		t.Fatalf("push url after get = %q, want fork URL", got.PushURL())
	}

	cleared, err := d.UpdateRepoForkURL(repo.ID, "")
	if err != nil {
		t.Fatalf("clear fork URL: %v", err)
	}
	if cleared.ForkURL != "" {
		t.Fatalf("fork url after clear = %q, want empty", cleared.ForkURL)
	}
	if cleared.PushURL() != cleared.UpstreamURL {
		t.Fatalf("push url after clear = %q, want upstream %q", cleared.PushURL(), cleared.UpstreamURL)
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

func TestRepoGitHubContextRoundTripAndClear(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/home/user/context-project", "https://github.com/parent/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	ctx := &repoexec.GitHubContext{
		Version:          1,
		GHPath:           filepath.Join("/opt", "tools", "gh"),
		GitPath:          filepath.Join("/opt", "tools", "git"),
		GHConfigDir:      filepath.Join("/home", "user", ".config", "gh-work"),
		Host:             "github.com",
		ExpectedLogin:    "work-user",
		GitProtocol:      "https",
		CredentialHelper: "gh",
		CommitAuthor: repoexec.CommitAuthor{
			Name:  "Work User",
			Email: "work@example.test",
		},
		Label: "work",
	}

	updated, err := d.UpdateRepoGitHubContext(repo.ID, ctx)
	if err != nil {
		t.Fatalf("set GitHub context: %v", err)
	}
	if updated.GitHubContext == nil || updated.GitHubContext.ExpectedLogin != "work-user" {
		t.Fatalf("updated context = %#v", updated.GitHubContext)
	}
	got, err := d.GetRepo(repo.ID)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got.GitHubContext == nil || *got.GitHubContext != *ctx {
		t.Fatalf("round-trip context = %#v, want %#v", got.GitHubContext, ctx)
	}

	cleared, err := d.UpdateRepoGitHubContext(repo.ID, nil)
	if err != nil {
		t.Fatalf("clear GitHub context: %v", err)
	}
	if cleared.GitHubContext != nil {
		t.Fatalf("context after clear = %#v, want nil", cleared.GitHubContext)
	}
}

func TestRepoGitHubContextRejectsCredentialLikeDurableValues(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/home/user/secret-context-project", "https://github.com/parent/project.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	selected := &repoexec.GitHubContext{Label: "ghp_credential-sentinel"}
	if _, err := d.UpdateRepoGitHubContext(repo.ID, selected); err == nil {
		t.Fatal("expected credential-like context to be rejected")
	} else if strings.Contains(err.Error(), "ghp_credential-sentinel") {
		t.Fatalf("error leaked rejected value: %v", err)
	}
	got, err := d.GetRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GitHubContext != nil {
		t.Fatalf("rejected context was persisted: %#v", got.GitHubContext)
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
