package gate

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

func TestRefreshRepoURLsSSHToHTTPS(t *testing.T) {
	ctx := context.Background()
	database, workDir := refreshFixture(t, "git@example.com:owner/project.git", "")
	gitTestCmd(t, workDir, "remote", "add", "origin", "https://example.com/owner/project.git")

	repo, err := database.GetRepoByPath(workDir)
	if err != nil {
		t.Fatal(err)
	}
	updated, changed, err := RefreshRepoURLs(ctx, database, repo)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !changed {
		t.Fatal("expected URL refresh")
	}
	if updated.UpstreamURL != "https://example.com/owner/project.git" || updated.ForkURL != "" {
		t.Fatalf("updated repo = %+v", updated)
	}
}

func TestRefreshRepoURLsRefreshesUpstreamAndForkTogether(t *testing.T) {
	ctx := context.Background()
	database, workDir := refreshFixture(t, "git@github.com:parent/project.git", "git@github.com:fork/project.git")
	gitTestCmd(t, workDir, "remote", "add", "origin", "https://github.com/parent/project.git")
	gitTestCmd(t, workDir, "remote", "add", "fork", "https://github.com/fork/project.git")
	repo, _ := database.GetRepoByPath(workDir)

	updated, changed, err := RefreshRepoURLs(ctx, database, repo)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !changed {
		t.Fatal("expected atomic upstream and fork refresh")
	}
	if updated.UpstreamURL != "https://github.com/parent/project.git" || updated.ForkURL != "https://github.com/fork/project.git" {
		t.Fatalf("updated repo = %+v", updated)
	}
}

func TestRefreshRepoURLsUnchangedIsNoOp(t *testing.T) {
	ctx := context.Background()
	const remote = "https://example.com/owner/project.git"
	database, workDir := refreshFixture(t, remote, "")
	gitTestCmd(t, workDir, "remote", "add", "origin", remote)
	repo, _ := database.GetRepoByPath(workDir)

	updated, changed, err := RefreshRepoURLs(ctx, database, repo)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if changed {
		t.Fatal("unchanged remote reported a replacement")
	}
	if !updated.URLsVerified {
		t.Fatal("unchanged validated remote was not marked verified for this run")
	}
	updated.URLsVerified = false
	if *updated != *repo {
		t.Fatalf("no-op changed persisted repo fields: got %+v want %+v", updated, repo)
	}
}

func TestRefreshRepoURLsFailurePreservesExactRegistration(t *testing.T) {
	tests := []struct {
		name       string
		origin     string
		fork       string
		addRemotes func(*testing.T, string)
		wantReason RefreshFailureReason
	}{
		{
			name:       "missing origin",
			origin:     "git@example.com:owner/project.git",
			wantReason: RefreshRemoteUnreadable,
		},
		{
			name:   "multiple origin URLs",
			origin: "git@example.com:owner/project.git",
			addRemotes: func(t *testing.T, dir string) {
				gitTestCmd(t, dir, "remote", "add", "origin", "https://example.com/owner/project.git")
				gitTestCmd(t, dir, "config", "--add", "remote.origin.url", "ssh://git@example.com/owner/project.git")
			},
			wantReason: RefreshAmbiguousRemote,
		},
		{
			name:   "malformed origin",
			origin: "git@example.com:owner/project.git",
			addRemotes: func(t *testing.T, dir string) {
				gitTestCmd(t, dir, "remote", "add", "origin", "https://example.com")
			},
			wantReason: RefreshInvalidRemote,
		},
		{
			name:   "credential-bearing origin",
			origin: "git@example.com:owner/project.git",
			addRemotes: func(t *testing.T, dir string) {
				gitTestCmd(t, dir, "remote", "add", "origin", "https://user:secret@example.com/owner/project.git")
			},
			wantReason: RefreshInvalidRemote,
		},
		{
			name:   "missing fork remote",
			origin: "https://example.com/parent/project.git",
			fork:   "git@example.com:fork/project.git",
			addRemotes: func(t *testing.T, dir string) {
				gitTestCmd(t, dir, "remote", "add", "origin", "https://example.com/parent/project.git")
			},
			wantReason: RefreshRemoteUnreadable,
		},
		{
			name:   "ambiguous fork remote",
			origin: "https://example.com/parent/project.git",
			fork:   "git@example.com:fork/project.git",
			addRemotes: func(t *testing.T, dir string) {
				gitTestCmd(t, dir, "remote", "add", "origin", "https://example.com/parent/project.git")
				gitTestCmd(t, dir, "remote", "add", "fork-a", "https://example.com/fork/project.git")
				gitTestCmd(t, dir, "remote", "add", "fork-b", "ssh://git@example.com/fork/project.git")
			},
			wantReason: RefreshAmbiguousRemote,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, workDir := refreshFixture(t, tt.origin, tt.fork)
			if tt.addRemotes != nil {
				tt.addRemotes(t, workDir)
			}
			before, _ := database.GetRepoByPath(workDir)
			_, _, err := RefreshRepoURLs(context.Background(), database, before)
			if err == nil {
				t.Fatal("expected refresh failure")
			}
			if got := ReasonForRefreshFailure(err); got != tt.wantReason {
				t.Fatalf("reason = %q, want %q (err %v)", got, tt.wantReason, err)
			}
			after, getErr := database.GetRepo(before.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if *after != *before {
				t.Fatalf("registration changed on failure: before %+v after %+v", before, after)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "https://") || strings.Contains(err.Error(), "git@") {
				t.Fatalf("refresh error exposed sensitive URL material: %q", err)
			}
		})
	}
}

func gitTestCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func refreshFixture(t *testing.T, upstream, fork string) (*db.DB, string) {
	t.Helper()
	workDir := t.TempDir()
	gitTestCmd(t, workDir, "init")
	database, err := db.Open(t.TempDir() + "/state.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.InsertRepoWithIDAndFork("repo", workDir, upstream, fork, "main"); err != nil {
		t.Fatal(err)
	}
	return database, workDir
}
