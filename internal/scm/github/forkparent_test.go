package github

import (
	"context"
	"strings"
	"testing"
)

func TestRepoParent_ForkReturnsParentSlug(t *testing.T) {
	t.Parallel()

	cmd := githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/fork-owner/no-mistakes": {
			stdout: `{"fork":true,"parent":{"full_name":"parent-owner/no-mistakes"}}`,
		},
	})

	parentSlug, isFork, err := RepoParent(context.Background(), cmd, "", "git@github.com:fork-owner/no-mistakes.git")
	if err != nil {
		t.Fatalf("RepoParent() error = %v", err)
	}
	if !isFork {
		t.Fatal("RepoParent() isFork = false, want true")
	}
	if parentSlug != "parent-owner/no-mistakes" {
		t.Fatalf("RepoParent() parentSlug = %q, want %q", parentSlug, "parent-owner/no-mistakes")
	}
}

func TestRepoParent_NonForkReturnsNoParent(t *testing.T) {
	t.Parallel()

	cmd := githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/owner/no-mistakes": {
			stdout: `{"fork":false,"parent":null}`,
		},
	})

	parentSlug, isFork, err := RepoParent(context.Background(), cmd, "", "https://github.com/owner/no-mistakes")
	if err != nil {
		t.Fatalf("RepoParent() error = %v", err)
	}
	if isFork {
		t.Fatal("RepoParent() isFork = true, want false")
	}
	if parentSlug != "" {
		t.Fatalf("RepoParent() parentSlug = %q, want empty", parentSlug)
	}
}

func TestRepoParent_UsesHostnameFlagForGHE(t *testing.T) {
	t.Parallel()

	cmd := githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --hostname ghe.example.com repos/owner/repo": {
			stdout: `{"fork":true,"parent":{"full_name":"parent/repo"}}`,
		},
	})

	parentSlug, isFork, err := RepoParent(context.Background(), cmd, "ghe.example.com", "https://ghe.example.com/owner/repo.git")
	if err != nil {
		t.Fatalf("RepoParent() error = %v", err)
	}
	if !isFork || parentSlug != "parent/repo" {
		t.Fatalf("RepoParent() = (%q, %v), want (\"parent/repo\", true)", parentSlug, isFork)
	}
}

func TestRepoParent_GHFailureSurfacesStderr(t *testing.T) {
	t.Parallel()

	cmd := githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/owner/repo": {stderr: "gh: not authenticated", code: 1},
	})

	_, _, err := RepoParent(context.Background(), cmd, "", "https://github.com/owner/repo")
	if err == nil {
		t.Fatal("RepoParent() error = nil, want gh failure")
	}
	if !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("RepoParent() error = %v, want stderr detail", err)
	}
}

func TestRepoParent_UnresolvableSlugFails(t *testing.T) {
	t.Parallel()

	_, _, err := RepoParent(context.Background(), githubTestCmdFactory(nil), "", "not-a-url")
	if err == nil {
		t.Fatal("RepoParent() error = nil, want a slug resolution failure")
	}
}

func TestRepoParent_UnparseableResponseFails(t *testing.T) {
	t.Parallel()

	cmd := githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/owner/repo": {stdout: "not json"},
	})

	_, _, err := RepoParent(context.Background(), cmd, "", "https://github.com/owner/repo")
	if err == nil {
		t.Fatal("RepoParent() error = nil, want a parse failure")
	}
}
