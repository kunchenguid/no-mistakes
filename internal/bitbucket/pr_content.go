package bitbucket

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Bitbucket's read shape is not its write shape: summary.raw is the text as
// typed, while summary.html is rendered. Read only that raw field; never
// infer an empty body from an absent summary or use rendered HTML.
func (h *Host) GetPRContent(ctx context.Context, pr *scm.PR) (scm.PRContent, error) {
	if pr == nil {
		return scm.PRContent{}, fmt.Errorf("missing Bitbucket pull identity")
	}
	id, err := strconv.Atoi(pr.Number)
	if err != nil || id <= 0 {
		return scm.PRContent{}, fmt.Errorf("invalid Bitbucket pull number")
	}
	var raw struct {
		ID      int    `json:"id"`
		Title   string `json:"title"`
		Summary *struct {
			Raw *string `json:"raw"`
		} `json:"summary"`
	}
	if err := h.client.doJSON(ctx, http.MethodGet, fmt.Sprintf("%s/%d", repoPRPath(h.repo), id), nil, nil, &raw); err != nil {
		return scm.PRContent{}, err
	}
	if raw.ID != id || strings.TrimSpace(raw.Title) == "" || raw.Summary == nil || raw.Summary.Raw == nil {
		return scm.PRContent{}, fmt.Errorf("Bitbucket pull: incomplete or mismatched raw content")
	}
	return scm.PRContent{Title: raw.Title, Body: *raw.Summary.Raw}, nil
}

var _ scm.PRContentReader = (*Host)(nil)
