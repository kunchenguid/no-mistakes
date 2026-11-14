package db

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

func TestRunCommitIdentityIsImmutableAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo(t.TempDir(), "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	wants := map[string]git.CommitIdentity{}
	for _, name := range []string{"first", "second"} {
		run, err := d.InsertRun(repo.ID, name, "head", "base")
		if err != nil {
			t.Fatal(err)
		}
		identity := git.CommitIdentity{AuthorName: name, AuthorEmail: name + "@example.test", CommitterName: name, CommitterEmail: name + "@example.test"}
		if err := d.BindRunCommitIdentity(run.ID, identity); err != nil {
			t.Fatal(err)
		}
		if err := d.BindRunCommitIdentity(run.ID, identity); err == nil {
			t.Fatal("identity binding was replaceable")
		}
		wants[run.ID] = identity
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for id, want := range wants {
		got, err := d.RunCommitIdentity(id)
		if err != nil || got != want {
			t.Fatalf("reopened identity = %+v, %v, want %+v", got, err, want)
		}
	}
	if _, err := d.RunCommitIdentity("unbound"); err == nil {
		t.Fatal("unbound run gained an identity")
	}
}
