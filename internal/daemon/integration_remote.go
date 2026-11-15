package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/integration"
)

func integrationRepo(ctx context.Context, registered *db.Repo, destination *integration.Destination) (*db.Repo, error) {
	if destination == nil {
		return registered, nil
	}
	if _, err := destination.RuntimeURL(ctx, registered.WorkingPath); err != nil {
		return nil, err
	}
	copy := *registered
	copy.Integration = destination
	copy.UpstreamURL = destination.URL
	copy.DefaultBranch = destination.DefaultBranch
	copy.URLsVerified = true
	return &copy, nil
}

func integrationBranchRef(repo *db.Repo, branch string) string {
	if repo.Integration != nil {
		return repo.Integration.Ref(branch)
	}
	return "refs/remotes/origin/" + branch
}

func fetchRecoveredIntegrationBranch(ctx context.Context, workDir string, repo *db.Repo) error {
	if repo.Integration != nil {
		return fetchRunDefaultBranch(ctx, workDir, repo)
	}
	return fetchRecoveredRemoteBranch(ctx, workDir, "origin", repo.DefaultBranch)
}

func matchingIntegrationRequest(ctx context.Context, run *db.Run) error {
	requested := integration.RequestedRemote(ctx)
	if !integration.MatchesRemote(integration.RemoteName(run.Integration), requested) {
		return fmt.Errorf("run already has a different integration destination; omit --base-remote to reattach")
	}
	return nil
}

func inheritIntegrationPRURL(ctx context.Context, repo *db.Repo, run *db.Run) (string, error) {
	prURL := inheritablePRURL(run)
	if prURL == "" {
		return "", nil
	}
	requested := integration.RequestedRemote(ctx)
	if requested == "" {
		requested = "origin"
	}
	if requested != integration.RemoteName(run.Integration) {
		return "", fmt.Errorf("cannot inherit an existing PR from another integration remote")
	}
	if run.Integration != nil {
		if _, err := run.Integration.RuntimeURL(ctx, repo.WorkingPath); err != nil {
			return "", err
		}
	}
	return prURL, nil
}

func (m *RunManager) checkBranchIntegrationPR(ctx context.Context, repo *db.Repo, branch string) error {
	runs, err := m.db.GetRunsByRepo(repo.ID)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Branch == branch && run.PRURL != nil && strings.TrimSpace(*run.PRURL) != "" {
			_, err := inheritIntegrationPRURL(ctx, repo, run)
			return err
		}
	}
	return nil
}

// Startup cannot derive this cleanup from worktree directories: a terminal
// run's worktree may have been reaped before its private refs were removed.
func (m *RunManager) cleanupTerminalIntegrationRefs(ctx context.Context) {
	runs, err := m.db.GetIntegrationRuns()
	if err != nil {
		slog.Warn("cannot list integration ref runs", "error", err)
		return
	}
	for _, run := range runs {
		if run.Status.Terminal() {
			m.cleanupIntegrationRefs(ctx, run, m.paths.RepoDir(run.RepoID))
		}
	}
}

func (m *RunManager) cleanupIntegrationRefs(ctx context.Context, run *db.Run, gateDir string) {
	if run == nil || run.Integration == nil || !run.Status.Terminal() {
		return
	}
	if err := run.Integration.Validate(); err != nil {
		return
	}
	runs, err := m.db.GetRunsByRepo(run.RepoID)
	if err != nil {
		slog.Warn("cannot check integration ref owners", "run_id", run.ID, "error", err)
		return
	}
	for _, other := range runs {
		if !other.Status.Terminal() && other.Integration != nil && other.Integration.Key == run.Integration.Key {
			return
		}
	}
	refs, err := git.RunBare(ctx, gateDir, "for-each-ref", "--format=%(refname)", run.Integration.Ref(""))
	if err != nil {
		slog.Warn("cannot list integration refs", "run_id", run.ID, "error", err)
		return
	}
	for _, ref := range strings.Fields(refs) {
		if _, err := git.RunBare(ctx, gateDir, "update-ref", "--no-deref", "-d", ref); err != nil {
			slog.Warn("cannot remove integration ref", "run_id", run.ID, "error", err)
		}
	}
}
