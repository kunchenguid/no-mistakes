package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

func TestSubmissionReconciliationBase(t *testing.T) {
	for _, tc := range []struct {
		name, trusted, pushed, requested, want string
	}{
		{name: "default", want: "main"},
		{name: "trusted", trusted: "pr:\n  base_branch: integration\n", want: "integration"},
		{name: "untrusted", pushed: "pr:\n  base_branch: other\n", want: "main"},
		{name: "trusted_wins", trusted: "pr:\n  base_branch: integration\n", pushed: "pr:\n  base_branch: other\n", want: "integration"},
		{name: "opt_in", trusted: "allow_repo_commands: true\npr:\n  base_branch: integration\n", pushed: "pr:\n  base_branch: other\n", want: "other"},
		{name: "override", trusted: "pr:\n  base_branch: integration\n", requested: "run-base", want: "run-base"},
		{name: "invalid", trusted: "pr: [\n", want: ""},
		{name: "unreadable", want: ""},
	} {
		for _, location := range []string{"root", "subdirectory"} {
			t.Run(tc.name+"/"+location, func(t *testing.T) {
				work := t.TempDir()
				cliGit(t, work, "init", "-b", "main")
				cliGit(t, work, "config", "user.name", "fixture")
				cliGit(t, work, "config", "user.email", "fixture@localhost")
				if tc.trusted != "" {
					if err := os.WriteFile(filepath.Join(work, ".no-mistakes.yaml"), []byte(tc.trusted), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				cliGit(t, work, "add", "-A")
				cliGit(t, work, "commit", "--allow-empty", "-m", "trusted config")
				upstream := t.TempDir()
				cliGit(t, upstream, "init", "--bare")
				cliGit(t, work, "remote", "add", "origin", upstream)
				cliGit(t, work, "push", "origin", "main")
				cliGit(t, work, "checkout", "-b", "feature")
				if tc.pushed != "" {
					if err := os.WriteFile(filepath.Join(work, ".no-mistakes.yaml"), []byte(tc.pushed), 0o644); err != nil {
						t.Fatal(err)
					}
					cliGit(t, work, "add", "-A")
					cliGit(t, work, "commit", "-m", "pushed config")
				}
				head := cliGit(t, work, "rev-parse", "HEAD")
				if tc.name == "unreadable" {
					cliGit(t, work, "remote", "set-url", "origin", filepath.Join(work, "missing.git"))
				}
				cwd := work
				if location == "subdirectory" {
					cwd = filepath.Join(work, "subdirectory")
					if err := os.Mkdir(cwd, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				t.Chdir(cwd)
				env := &axiEnv{repo: &db.Repo{DefaultBranch: "main"}}
				got := submissionReconciliationUpstream(context.Background(), env, head, tc.requested)
				if got.BaseBranch != tc.want {
					t.Fatalf("selected base = %q, want %q", got.BaseBranch, tc.want)
				}
				if tc.want != "" && got.URL != upstream {
					t.Fatalf("selected upstream = %q, want %q", got.URL, upstream)
				}
			})
		}
	}
}
