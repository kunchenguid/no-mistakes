package daemon

import (
	"context"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
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
	if requested == "" {
		return nil
	}
	if run.Integration == nil || run.Integration.Remote != requested {
		return fmt.Errorf("run already has a different integration destination; omit --base-remote to reattach")
	}
	return nil
}

func runIntegrationRemote(run *db.Run) string {
	if run.Integration != nil {
		return run.Integration.Remote
	}
	return "origin"
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
	if requested != runIntegrationRemote(run) {
		return "", fmt.Errorf("cannot inherit an existing PR from another integration remote")
	}
	if run.Integration != nil {
		if _, err := run.Integration.RuntimeURL(ctx, repo.WorkingPath); err != nil {
			return "", err
		}
	}
	return prURL, nil
}
