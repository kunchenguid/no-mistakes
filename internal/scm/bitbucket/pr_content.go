package bitbucket

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// GetPRContent reads the current title and raw description of an existing
// PR. twg normalizes Bitbucket's summary.raw/summary.html split into a single
// top-level "description" string field (confirmed by twg's own documented
// agent-field presets for `pull-requests get`/`query`/`create`/`update`,
// which use "description" throughout rather than "summary.raw"); read only
// that field, never infer an empty body from an absent one.
func (h *Host) GetPRContent(ctx context.Context, pr *scm.PR) (scm.PRContent, error) {
	if pr == nil {
		return scm.PRContent{}, fmt.Errorf("missing Bitbucket pull identity")
	}
	id, err := strconv.Atoi(pr.Number)
	if err != nil || id <= 0 {
		return scm.PRContent{}, fmt.Errorf("invalid Bitbucket pull number")
	}
	_, trimmed, err := h.getPR(ctx, pr.Number)
	if err != nil {
		return scm.PRContent{}, err
	}
	var raw struct {
		ID          int     `json:"id"`
		Title       string  `json:"title"`
		Description *string `json:"description"`
	}
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return scm.PRContent{}, fmt.Errorf("twg bb pull-requests get: invalid JSON output: %s", strings.TrimSpace(string(trimmed)))
	}
	if raw.ID != id || strings.TrimSpace(raw.Title) == "" || raw.Description == nil {
		return scm.PRContent{}, fmt.Errorf("Bitbucket pull: incomplete or mismatched raw content")
	}
	return scm.PRContent{Title: raw.Title, Body: *raw.Description}, nil
}

var _ scm.PRContentReader = (*Host)(nil)
