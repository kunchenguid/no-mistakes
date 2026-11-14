package azuredevops

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func (h *Host) GetPRRepositoryContent(ctx context.Context, number string) (scm.PRRepositoryContent, error) {
	got, err := h.showPR(ctx, &scm.PR{Number: number})
	if err != nil {
		return scm.PRRepositoryContent{}, err
	}
	if fmt.Sprint(got.PullRequestID) != number || strings.TrimSpace(got.Title) == "" || got.Description == nil {
		return scm.PRRepositoryContent{}, fmt.Errorf("az repos pr show: incomplete or mismatched content")
	}
	// Reuse the adapter's existing proof of organization/project/repository,
	// including contradictory metadata. Publication's raw read is unchanged.
	if err := h.validateListedPR(*got); err != nil {
		return scm.PRRepositoryContent{}, fmt.Errorf("az repository identity: %w", err)
	}
	org, project, repo := h.org, got.Repository.Project.Name, got.Repository.Name
	if got.Repository.WebURL != "" {
		var ok bool
		org, project, repo, ok = ParseRemote(got.Repository.WebURL)
		if !ok {
			return scm.PRRepositoryContent{}, fmt.Errorf("az repository identity is malformed")
		}
	}
	identity := RepositoryIdentity(org, project, repo)
	return scm.PRRepositoryContent{Content: scm.PRContent{Title: got.Title, Body: *got.Description}, Repository: identity}, nil
}

func RepositoryIdentity(org, project, repo string) scm.RepositoryIdentity {
	return scm.RepositoryIdentity{Host: "dev.azure.com", Path: azureOrganizationName(org) + "/" + project + "/" + repo}
}

var _ scm.PRRepositoryContentReader = (*Host)(nil)
