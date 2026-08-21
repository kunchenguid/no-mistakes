package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const refreshTestZeroSHA = "0000000000000000000000000000000000000000"

func TestRunStartRefreshesCloneURLWithoutMutatingRemotes(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "refresh-success")
	const oldURL = "git@example.com:owner/project.git"
	const currentURL = "https://example.com/owner/project.git"
	if _, err := database.ReplaceRepoURLs(repo.ID, oldURL, ""); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "remote", "add", "origin", currentURL)
	gitCmd(t, p.RepoDir(repo.ID), "remote", "set-url", "origin", oldURL)
	gitCmd(t, p.RepoDir(repo.ID), "config", "url."+p.RepoDir(repo.ID)+".insteadOf", currentURL)

	seen := make(chan *db.Repo, 1)
	manager := NewRunManager(database, p, func() []pipeline.Step {
		return []pipeline.Step{&captureRefreshRepoStep{seen: seen}}
	})
	t.Cleanup(manager.Shutdown)
	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "refresh repository URL", "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
	}

	got := <-seen
	if got.UpstreamURL != currentURL || !got.URLsVerified {
		t.Fatalf("pipeline repo = %+v, want refreshed verified URL", got)
	}
	stored, err := database.GetRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.UpstreamURL != currentURL {
		t.Fatalf("stored upstream = %q, want %q", stored.UpstreamURL, currentURL)
	}
	if cloneURL, err := git.GetConfiguredRemoteURL(context.Background(), repo.WorkingPath, "origin"); err != nil || cloneURL != currentURL {
		t.Fatalf("clone origin = %q, %v; want unchanged %q", cloneURL, err, currentURL)
	}
	if gateURL, err := git.GetConfiguredRemoteURL(context.Background(), p.RepoDir(repo.ID), "origin"); err != nil || gateURL != oldURL {
		t.Fatalf("gate origin = %q, %v; want unchanged %q", gateURL, err, oldURL)
	}
	t.Logf(
		"observable run-start result: status=%s persisted_upstream=%q clone_origin=%q gate_origin=%q clone_and_gate_remotes_unchanged=true",
		types.RunCompleted,
		stored.UpstreamURL,
		currentURL,
		oldURL,
	)
}

func TestRunStartURLRefreshFailuresWarnSafelyAndContinueWithOldRegistration(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *paths.Paths, *db.DB, *db.Repo)
	}{
		{name: "missing origin"},
		{
			name: "malformed origin",
			setup: func(t *testing.T, _ *paths.Paths, _ *db.DB, repo *db.Repo) {
				gitCmd(t, repo.WorkingPath, "remote", "add", "origin", "https://example.com")
			},
		},
		{
			name: "credential-bearing origin",
			setup: func(t *testing.T, _ *paths.Paths, _ *db.DB, repo *db.Repo) {
				gitCmd(t, repo.WorkingPath, "remote", "add", "origin", "https://user:top-secret@example.com/owner/project.git")
			},
		},
		{
			name: "ambiguous fork remotes",
			setup: func(t *testing.T, _ *paths.Paths, database *db.DB, repo *db.Repo) {
				if _, err := database.ReplaceRepoURLs(repo.ID, repo.UpstreamURL, "git@example.com:fork/project.git"); err != nil {
					t.Fatal(err)
				}
				repo.ForkURL = "git@example.com:fork/project.git"
				gitCmd(t, repo.WorkingPath, "remote", "add", "origin", "https://example.com/parent/project.git")
				gitCmd(t, repo.WorkingPath, "remote", "add", "fork-a", "https://example.com/fork/project.git")
				gitCmd(t, repo.WorkingPath, "remote", "add", "fork-b", "ssh://git@example.com/fork/project.git")
			},
		},
		{
			name: "database write failure",
			setup: func(t *testing.T, p *paths.Paths, _ *db.DB, repo *db.Repo) {
				gitCmd(t, repo.WorkingPath, "remote", "add", "origin", "https://example.com/owner/project.git")
				raw, err := sql.Open("sqlite", p.DB())
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				if _, err := raw.Exec(`CREATE TRIGGER reject_run_start_repo_url_update BEFORE UPDATE OF upstream_url, fork_url ON repos BEGIN SELECT RAISE(FAIL, 'injected URL write failure'); END`); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NM_DEMO", "1")
			p, database := newRefreshRunFixture(t)
			repo, head := setupTestGitRepo(t, p, database, "refresh-failure")
			before, err := database.GetRepo(repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tt.setup != nil {
				tt.setup(t, p, database, repo)
				before, err = database.GetRepo(repo.ID)
				if err != nil {
					t.Fatal(err)
				}
			}

			var logs bytes.Buffer
			oldLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
			defer slog.SetDefault(oldLogger)

			seen := make(chan *db.Repo, 1)
			manager := NewRunManager(database, p, func() []pipeline.Step {
				return []pipeline.Step{&captureRefreshRepoStep{seen: seen}}
			})
			t.Cleanup(manager.Shutdown)
			runID, err := manager.startRun(context.Background(), before, "main", head, refreshTestZeroSHA, "test", nil, "refresh failure must fail open", "")
			if err != nil {
				t.Fatalf("ordinary run did not continue: %v\nlogs: %s", err, logs.String())
			}
			if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
				t.Fatalf("run status = %s, error = %v\nlogs: %s", run.Status, run.Error, logs.String())
			}
			pipelineRepo := <-seen
			if pipelineRepo.UpstreamURL != before.UpstreamURL || pipelineRepo.ForkURL != before.ForkURL || pipelineRepo.URLsVerified {
				t.Fatalf("pipeline did not retain prior registration: got %+v want %+v", pipelineRepo, before)
			}
			after, err := database.GetRepo(repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.UpstreamURL != before.UpstreamURL || after.ForkURL != before.ForkURL {
				t.Fatalf("stored registration changed: before %+v after %+v", before, after)
			}
			warning := logs.String()
			if !strings.Contains(warning, "repository URL refresh skipped") || !strings.Contains(warning, "reason=") {
				t.Fatalf("missing concise refresh warning: %q", warning)
			}
			for _, sensitive := range []string{"top-secret", "user:", "https://", "git@", repo.WorkingPath} {
				if strings.Contains(warning, sensitive) {
					t.Fatalf("warning exposed sensitive remote material %q: %s", sensitive, warning)
				}
			}
			t.Logf(
				"observable fail-open result: status=%s prior_registration_preserved=true warning=%q",
				types.RunCompleted,
				strings.TrimSpace(warning),
			)
		})
	}
}

type captureRefreshRepoStep struct {
	seen chan<- *db.Repo
}

func (s *captureRefreshRepoStep) Name() types.StepName { return types.StepReview }
func (s *captureRefreshRepoStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	copy := *sctx.Repo
	s.seen <- &copy
	return &pipeline.StepOutcome{}, nil
}

func newRefreshRunFixture(t *testing.T) (*paths.Paths, *db.DB) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return p, database
}
