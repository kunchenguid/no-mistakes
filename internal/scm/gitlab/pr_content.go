package gitlab

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// GetPRContent reads glab's single-MR JSON description, never rendered HTML.
// A missing/null description is unproven, not an empty author narrative.
func (h *Host) GetPRContent(ctx context.Context, pr *scm.PR) (scm.PRContent, error) {
	if pr == nil {
		return scm.PRContent{}, fmt.Errorf("missing merge request identity")
	}
	id := pr.Number
	if id == "" {
		id, _ = scm.ExtractPRNumber(pr.URL)
	}
	if id == "" {
		return scm.PRContent{}, fmt.Errorf("missing merge request number")
	}
	mr, err := h.viewMR(ctx, id)
	if err != nil {
		return scm.PRContent{}, err
	}
	if fmt.Sprint(mr.IID) != id || strings.TrimSpace(mr.Title) == "" || mr.Description == nil {
		return scm.PRContent{}, fmt.Errorf("glab mr view: incomplete or mismatched raw content")
	}
	return scm.PRContent{Title: mr.Title, Body: *mr.Description}, nil
}

var _ scm.PRContentReader = (*Host)(nil)
