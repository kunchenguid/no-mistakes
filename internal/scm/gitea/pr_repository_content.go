package gitea

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func (h *Host) GetPRRepositoryContent(ctx context.Context, number string) (scm.PRRepositoryContent, error) {
	n, err := strconv.Atoi(number)
	if err != nil || n <= 0 {
		return scm.PRRepositoryContent{}, fmt.Errorf("invalid Gitea PR number")
	}
	owner, repo, ok := splitOwnerRepo(h.repoSlug)
	if !ok {
		return scm.PRRepositoryContent{}, fmt.Errorf("invalid Gitea repository")
	}
	_, account, err := h.begin(ctx)
	if err != nil {
		return scm.PRRepositoryContent{}, err
	}
	args := append([]string{"api"}, account...)
	out, err := h.cmd(ctx, "tea", append(args, fmt.Sprintf("/repos/%s/%s/pulls/%s", owner, repo, number))...).Output()
	if err != nil {
		return scm.PRRepositoryContent{}, fmt.Errorf("tea api pull content: %w", err)
	}
	var raw struct {
		Number int     `json:"number"`
		Title  string  `json:"title"`
		Body   *string `json:"body"`
		URL    string  `json:"html_url"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return scm.PRRepositoryContent{}, fmt.Errorf("parse tea pull: %w", err)
	}
	if raw.Number != n || strings.TrimSpace(raw.Title) == "" || raw.Body == nil {
		return scm.PRRepositoryContent{}, fmt.Errorf("tea api pull: incomplete or mismatched content")
	}
	return scm.PRRepositoryContent{Content: scm.PRContent{Title: raw.Title, Body: *raw.Body}, Repository: scm.RepositoryIdentityFromPRURL(raw.URL, "/pulls/", number)}, nil
}

var _ scm.PRRepositoryContentReader = (*Host)(nil)
