package origin

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func (h *Host) GetPRRepositoryContent(ctx context.Context, number string) (scm.PRRepositoryContent, error) {
	view, err := h.viewPR(ctx, &scm.PR{Number: number})
	if err != nil {
		return scm.PRRepositoryContent{}, err
	}
	if view.number() != number || strings.TrimSpace(view.Title) == "" || view.Description == nil {
		return scm.PRRepositoryContent{}, fmt.Errorf("origin pr view: incomplete or mismatched content")
	}
	identity := scm.RepositoryIdentityFromPRURL(view.URL, "/pull/", number)
	// Origin's git and web routes have different authorities and the web
	// route carries /codebase/, as RepoSlug documents.
	if identity.Host == "cursor.com" && strings.HasPrefix(identity.Path, "codebase/") {
		identity.Host = "origin.cursor.com"
		identity.Path = strings.TrimPrefix(identity.Path, "codebase/")
	}
	return scm.PRRepositoryContent{Content: scm.PRContent{Title: view.Title, Body: *view.Description}, Repository: identity}, nil
}

var _ scm.PRRepositoryContentReader = (*Host)(nil)
