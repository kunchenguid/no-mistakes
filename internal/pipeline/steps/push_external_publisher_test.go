package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

const externalPublisherYAML = `push:
  external_publisher:
    paths: [.shipping-gate.json]
    reason: The shipping gate must stamp the commit before publication.
    remedy: Recover the validated head and publish with the shipping gate.
`

// Exercise both callers of the shared publication boundary. The submitted
// branch removes the policy, including when pushed commands are allowed.
func TestPublication_ExternalPublisher(t *testing.T) {
	for _, repair := range []bool{false, true} {
		for _, tc := range []struct {
			name, trusted                 string
			marker, refuse, allowCommands bool
		}{
			{name: "trusted declaration", trusted: externalPublisherYAML, marker: true, refuse: true},
			{name: "commands opt-in cannot disable", trusted: "allow_repo_commands: true\n" + externalPublisherYAML, marker: true, refuse: true, allowCommands: true},
			{name: "nonmatching declaration", trusted: externalPublisherYAML},
			{name: "unset", trusted: "{}\n", marker: true},
		} {
			name := "push/" + tc.name
			if repair {
				name = "repair/" + tc.name
			}
			t.Run(name, func(t *testing.T) {
				dir, _, _ := setupGitRepo(t)
				gitCmd(t, dir, "checkout", "main")
				if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(tc.trusted), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", ".no-mistakes.yaml")
				gitCmd(t, dir, "commit", "-m", "trusted publication policy")
				base := gitCmd(t, dir, "rev-parse", "HEAD")
				gitCmd(t, dir, "checkout", "feature")
				gitCmd(t, dir, "merge", "--no-edit", "main")
				if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte("push: {}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if tc.marker {
					if err := os.WriteFile(filepath.Join(dir, ".shipping-gate.json"), []byte("{}\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				gitCmd(t, dir, "add", "-A")
				gitCmd(t, dir, "commit", "-m", "submitted policy removal")
				submitted := gitCmd(t, dir, "rev-parse", "HEAD")
				upstream := t.TempDir()
				gitCmd(t, upstream, "init", "--bare")
				gitCmd(t, dir, "remote", "add", "origin", upstream)
				gitCmd(t, dir, "push", "origin", "main")
				sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, submitted, config.Commands{})
				sctx.Repo.UpstreamURL = upstream
				trusted, err := config.LoadRepoFromBytes([]byte(gitCmd(t, dir, "show", "main:.no-mistakes.yaml")))
				if err != nil {
					t.Fatal(err)
				}
				pushed, err := config.LoadRepoFromBytes([]byte(gitCmd(t, dir, "show", "HEAD:.no-mistakes.yaml")))
				if err != nil {
					t.Fatal(err)
				}
				global, err := config.LoadGlobalFromBytes([]byte("{}\n"))
				if err != nil {
					t.Fatal(err)
				}
				sctx.Config = config.Merge(global, config.EffectiveRepoConfig(pushed, trusted, tc.allowCommands))
				recordReviewApproval(t, sctx, submitted)
				// Model a pipeline correction made after the submitted commit.
				if err := os.WriteFile(filepath.Join(dir, "correction.txt"), []byte("pipeline correction\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", "correction.txt")
				gitCmd(t, dir, "commit", "-m", "pipeline correction")
				validated := gitCmd(t, dir, "rev-parse", "HEAD")
				env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")
				sctx.Env = env
				if repair {
					_, err = (&CIStep{}).publishRepair(sctx, validated)
				} else {
					_, err = (&PushStep{}).Execute(sctx)
				}
				if tc.refuse {
					if err == nil {
						t.Fatal("publication succeeded despite the trusted external publisher declaration")
					}
					for _, detail := range []string{".shipping-gate.json", "must stamp", "Recover the validated head"} {
						if !strings.Contains(err.Error(), detail) {
							t.Fatalf("refusal %q omits %q", err, detail)
						}
					}
					if refs := gitCmd(t, upstream, "for-each-ref", "--format=%(refname)", "refs/heads/feature"); refs != "" {
						t.Fatalf("refused head published: %s", refs)
					}
					if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != validated {
						t.Fatalf("validated head changed: %s", got)
					}
					calls, readErr := os.ReadFile(logFile)
					if readErr != nil {
						t.Fatal(readErr)
					}
					if len(calls) != 0 {
						t.Fatalf("forge called before refusal: %s", calls)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != validated {
					t.Fatalf("published %s, want %s", got, validated)
				}
			})
		}
	}
}

func TestPublication_ExternalPublisherRecovery(t *testing.T) {
	dir, base, submitted := setupGitRepo(t)
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, submitted, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	app := paths.WithRoot(t.TempDir())
	if err := app.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_HOME", app.Root())
	gate := app.RepoDir(sctx.Repo.ID)
	gitCmd(t, "", "init", "--bare", gate)
	gitCmd(t, gate, "fetch", dir, "feature:refs/heads/feature")
	work := filepath.Join(t.TempDir(), "pipeline")
	gitCmd(t, gate, "worktree", "add", "--detach", work, submitted)
	gitCmd(t, work, "config", "user.name", "Test")
	gitCmd(t, work, "config", "user.email", "test@test.com")
	gitCmd(t, work, "config", "commit.gpgsign", "false")
	for _, name := range []string{".shipping-gate.json", "correction.txt"} {
		if err := os.WriteFile(filepath.Join(work, name), []byte("pipeline correction\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, work, "add", "-A")
	gitCmd(t, work, "commit", "-m", "pipeline correction")
	validated := gitCmd(t, work, "rev-parse", "HEAD")
	recordReviewApproval(t, sctx, validated)
	global, err := config.LoadGlobalFromBytes([]byte("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := config.LoadRepoFromBytes([]byte(externalPublisherYAML))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Merge(global, config.EffectiveRepoConfig(nil, trusted, false))
	exec := pipeline.NewExecutor(sctx.DB, app, cfg, sctx.Agent, []pipeline.Step{&PushStep{}}, nil)
	if err := exec.Execute(context.Background(), sctx.Run, sctx.Repo, work); err == nil || !strings.Contains(err.Error(), "external publisher") {
		t.Fatalf("expected policy refusal, got %v", err)
	}
	// Remove the disposable pipeline worktree just as terminal cleanup does.
	gitCmd(t, gate, "worktree", "remove", work)
	service := &branchsync.Service{DB: sctx.DB, Repo: sctx.Repo, WorkDir: dir, GateDir: gate}
	state := service.Recover(context.Background(), false)
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != validated {
		t.Fatalf("normal recovery did not restore %s: got %s, state=%+v", validated, got, state)
	}
	if got := gitCmd(t, dir, "show", "HEAD:correction.txt"); got != "pipeline correction" {
		t.Fatalf("recovered correction = %q", got)
	}
}
