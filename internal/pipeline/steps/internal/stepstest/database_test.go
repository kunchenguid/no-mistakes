package stepstest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

// The fixture contract is a fresh, writable database per path, with durable
// state on reopen. Reusing a connection or hard-linking copies violates it.
func TestOpenNewDBIsolatesStateAndPreservesExistingFiles(t *testing.T) {
	t.Parallel()
	firstPath := filepath.Join(t.TempDir(), "first.db")
	first, err := OpenNewDB(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if _, err = first.InsertRepoWithID("repo", "first", "https://example.com/first", "main"); err != nil {
		t.Fatal(err)
	}
	second, err := OpenNewDB(filepath.Join(t.TempDir(), "second.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	repos, err := second.GetRepos()
	if err != nil || len(repos) != 0 {
		t.Fatalf("fresh database repos = %+v, %v; want empty", repos, err)
	}
	if _, err = second.InsertRepoWithID("repo", "second", "https://example.com/second", "develop"); err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := OpenNewDB(firstPath)
	if !os.IsExist(err) {
		if replacement != nil {
			_ = replacement.Close()
		}
		t.Fatalf("existing database replacement error = %v; want file exists", err)
	}
	reopened, err := db.Open(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	repo, err := reopened.GetRepo("repo")
	if err != nil {
		t.Fatal(err)
	}
	if repo == nil || repo.WorkingPath != "first" || repo.UpstreamURL != "https://example.com/first" || repo.DefaultBranch != "main" {
		t.Fatalf("reopened database lost its isolated state: %+v", repo)
	}
}
