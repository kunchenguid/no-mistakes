package bitbucket

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// GetPRRepositoryContent reads a numbered PR's text together with the
// repository identity the response itself reports, so a caller can refuse a
// PR that belongs to a different repository. Identity comes only from the
// response's own web link (never from the configured repo), and a response
// without a recognizable link yields an empty identity for the caller to
// refuse.
func (h *Host) GetPRRepositoryContent(ctx context.Context, number string) (scm.PRRepositoryContent, error) {
	id, err := strconv.Atoi(number)
	if err != nil || id <= 0 {
		return scm.PRRepositoryContent{}, fmt.Errorf("invalid Bitbucket PR number")
	}
	_, trimmed, err := h.getPR(ctx, number)
	if err != nil {
		return scm.PRRepositoryContent{}, err
	}
	var raw struct {
		ID          int     `json:"id"`
		Title       string  `json:"title"`
		Description *string `json:"description"`
		Links       struct {
			HTML struct {
				Href string `json:"href"`
			} `json:"html"`
		} `json:"links"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return scm.PRRepositoryContent{}, fmt.Errorf("twg bb pull-requests get: invalid JSON output: %s", strings.TrimSpace(string(trimmed)))
	}
	if raw.ID != id || strings.TrimSpace(raw.Title) == "" || raw.Description == nil {
		return scm.PRRepositoryContent{}, fmt.Errorf("Bitbucket pull: incomplete or mismatched content")
	}
	return scm.PRRepositoryContent{
		Content:    scm.PRContent{Title: raw.Title, Body: *raw.Description},
		Repository: scm.RepositoryIdentityFromPRURL(raw.Links.HTML.Href, "/pull-requests/", number),
	}, nil
}

var _ scm.PRRepositoryContentReader = (*Host)(nil)
