package bitbucket

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func (h *Host) GetPRRepositoryContent(ctx context.Context, number string) (scm.PRRepositoryContent, error) {
	id, err := strconv.Atoi(number)
	if err != nil || id <= 0 {
		return scm.PRRepositoryContent{}, fmt.Errorf("invalid Bitbucket PR number")
	}
	var raw struct {
		ID      int    `json:"id"`
		Title   string `json:"title"`
		Summary *struct {
			Raw *string `json:"raw"`
		} `json:"summary"`
		Links struct {
			HTML struct {
				Href string `json:"href"`
			} `json:"html"`
		} `json:"links"`
	}
	if err := h.client.doJSON(ctx, http.MethodGet, fmt.Sprintf("%s/%d", repoPRPath(h.repo), id), nil, nil, &raw); err != nil {
		return scm.PRRepositoryContent{}, err
	}
	if raw.ID != id || strings.TrimSpace(raw.Title) == "" || raw.Summary == nil || raw.Summary.Raw == nil {
		return scm.PRRepositoryContent{}, fmt.Errorf("Bitbucket pull: incomplete or mismatched content")
	}
	return scm.PRRepositoryContent{Content: scm.PRContent{Title: raw.Title, Body: *raw.Summary.Raw}, Repository: scm.RepositoryIdentityFromPRURL(raw.Links.HTML.Href, "/pull-requests/", number)}, nil
}

var _ scm.PRRepositoryContentReader = (*Host)(nil)
