package azuredevops

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// GetPRContent uses az's raw description, preserving whitespace and Unicode.
func (h *Host) GetPRContent(ctx context.Context, pr *scm.PR) (scm.PRContent, error) {
	got, err := h.showPR(ctx, pr)
	if err != nil {
		return scm.PRContent{}, err
	}
	if fmt.Sprint(got.PullRequestID) != h.prID(pr) || strings.TrimSpace(got.Title) == "" || got.Description == nil {
		return scm.PRContent{}, fmt.Errorf("az repos pr show: incomplete or mismatched raw content")
	}
	return scm.PRContent{Title: got.Title, Body: *got.Description}, nil
}

var _ scm.PRContentReader = (*Host)(nil)
