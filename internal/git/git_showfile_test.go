package git

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefExists(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	ok, err := RefExists(ctx, repo, "HEAD")
	if err != nil {
		t.Fatalf("RefExists HEAD: %v", err)
	}
	if !ok {
		t.Fatal("HEAD should exist")
	}

	ok, err = RefExists(ctx, repo, "origin/nonexistent")
	if err != nil {
		t.Fatalf("RefExists missing ref: %v", err)
	}
	if ok {
		t.Fatal("missing ref should not exist")
	}
}

func TestRefExistsBareUnderSafeBareRepositoryExplicit(t *testing.T) {
	setSafeBareRepositoryExplicit(t)
	ctx := context.Background()
	source := initTestRepo(t)
	bare := filepath.Join(t.TempDir(), "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	run(t, source, "git", "remote", "add", "gate", bare)
	run(t, source, "git", "push", "gate", "HEAD:refs/heads/main")

	if ok, err := RefExistsBare(ctx, bare, "refs/heads/main"); err != nil || !ok {
		t.Fatalf("RefExistsBare existing ref = %v, %v", ok, err)
	}
	if ok, err := RefExists(ctx, bare, "refs/heads/missing"); err != nil || ok {
		t.Fatalf("RefExists missing bare ref = %v, %v", ok, err)
	}
	want := run(t, source, "git", "rev-parse", "HEAD")
	got, err := ResolveRefBare(ctx, bare, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ResolveRefBare = %q, want %q", got, want)
	}
}

func TestShowFile_AtHEAD(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	content, err := ShowFile(ctx, repo, "HEAD", "README.md")
	if err != nil {
		t.Fatalf("ShowFile HEAD:README.md: %v", err)
	}
	if content != "# test" {
		t.Errorf("content = %q, want %q", content, "# test")
	}
}

func TestResolveRef(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)
	want := run(t, repo, "git", "rev-parse", "HEAD")

	got, err := ResolveRef(ctx, repo, "HEAD")
	if err != nil {
		t.Fatalf("ResolveRef HEAD: %v", err)
	}
	if got != want {
		t.Errorf("ResolveRef HEAD = %q, want %q", got, want)
	}
}

func TestResolveRef_MissingRef(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	if _, err := ResolveRef(ctx, repo, "origin/does-not-exist"); err == nil {
		t.Fatal("expected error resolving a missing ref")
	}
}

func TestShowFile_AtBranchRef(t *testing.T) {
	ctx := context.Background()
	src := initTestRepo(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	// The gate bare repo records the upstream as origin; the linked worktree
	// shares that config, so the daemon's `git fetch origin <branch>` works.
	if err := AddRemote(ctx, bare, "origin", bare); err != nil {
		t.Fatalf("add origin to bare: %v", err)
	}
	run(t, src, "git", "remote", "add", "origin", bare)
	run(t, src, "git", "push", "origin", "HEAD:refs/heads/main")

	// Fetch into a remote-tracking ref like the daemon does for the worktree.
	wt := filepath.Join(t.TempDir(), "worktree")
	sha := run(t, src, "git", "rev-parse", "HEAD")
	if err := WorktreeAdd(ctx, bare, wt, sha); err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}
	if err := FetchRemoteBranch(ctx, wt, "origin", "main"); err != nil {
		t.Fatalf("FetchRemoteBranch: %v", err)
	}

	content, err := ShowFile(ctx, wt, "origin/main", "README.md")
	if err != nil {
		t.Fatalf("ShowFile origin/main:README.md: %v", err)
	}
	if content != "# test" {
		t.Errorf("content = %q, want %q", content, "# test")
	}
}

func TestShowFile_AbsentPath(t *testing.T) {
	ctx := context.Background()
	repo := initTestRepo(t)

	_, err := ShowFile(ctx, repo, "HEAD", "does-not-exist.yaml")
	if err == nil {
		t.Fatal("expected error for absent path")
	}
	if !strings.Contains(err.Error(), "does-not-exist.yaml") {
		t.Errorf("error should mention the path: %v", err)
	}
}
