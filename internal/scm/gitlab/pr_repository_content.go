package gitlab

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func (h *Host) GetPRRepositoryContent(ctx context.Context, number string) (scm.PRRepositoryContent, error) {
	repo := h.projectPath
	if repo != "" && h.host != "" {
		repo = "https://" + h.host + "/" + repo
	}
	mr, err := h.viewMRInRepository(ctx, number, repo)
	if err != nil {
		return scm.PRRepositoryContent{}, err
	}
	if fmt.Sprint(mr.IID) != number || strings.TrimSpace(mr.Title) == "" || mr.Description == nil {
		return scm.PRRepositoryContent{}, fmt.Errorf("glab mr view: incomplete or mismatched content")
	}
	return scm.PRRepositoryContent{Content: scm.PRContent{Title: mr.Title, Body: *mr.Description}, Repository: scm.RepositoryIdentityFromPRURL(mr.WebURL, "/-/merge_requests/", number)}, nil
}

var _ scm.PRRepositoryContentReader = (*Host)(nil)
