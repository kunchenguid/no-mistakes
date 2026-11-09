package github

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func (h *Host) GetPRRepositoryContent(ctx context.Context, number string) (scm.PRRepositoryContent, error) {
	args := append([]string{"pr", "view", number}, h.repoArgs()...)
	args = append(args, "--json", "number,url,title,body")
	out, err := h.cmd(ctx, "gh", args...).Output()
	if err != nil {
		return scm.PRRepositoryContent{}, fmt.Errorf("gh pr view: %w", err)
	}
	var raw struct {
		Number      int
		URL         string
		Title, Body *string
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return scm.PRRepositoryContent{}, fmt.Errorf("parse gh pr view: %w", err)
	}
	if fmt.Sprint(raw.Number) != number || raw.Title == nil || raw.Body == nil {
		return scm.PRRepositoryContent{}, fmt.Errorf("gh pr view: incomplete or mismatched content")
	}
	return scm.PRRepositoryContent{Content: scm.PRContent{Title: *raw.Title, Body: *raw.Body}, Repository: scm.RepositoryIdentityFromPRURL(raw.URL, "/pull/", number)}, nil
}

var _ scm.PRRepositoryContentReader = (*Host)(nil)
