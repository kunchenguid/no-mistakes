package forgejo

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func (h *Host) GetPRRepositoryContent(ctx context.Context, number string) (scm.PRRepositoryContent, error) {
	n, err := strconv.Atoi(number)
	if err != nil || n <= 0 {
		return scm.PRRepositoryContent{}, fmt.Errorf("invalid Forgejo PR number")
	}
	var response struct {
		Status int `json:"status"`
		Data   *struct {
			Number int     `json:"number"`
			Title  string  `json:"title"`
			Body   *string `json:"body"`
			URL    string  `json:"html_url"`
		} `json:"data"`
	}
	if err := h.runJSON(ctx, "api", []string{"GET", "repos/" + h.repository + "/pulls/" + number}, &response); err != nil {
		return scm.PRRepositoryContent{}, err
	}
	if response.Status != 200 || response.Data == nil || strings.TrimSpace(response.Data.Title) == "" || response.Data.Body == nil {
		return scm.PRRepositoryContent{}, fmt.Errorf("forgejo-axi api: incomplete raw pull content")
	}
	if err := h.validateOutputPRNumber(number, response.Data.Number); err != nil {
		return scm.PRRepositoryContent{}, err
	}
	return scm.PRRepositoryContent{Content: scm.PRContent{Title: response.Data.Title, Body: *response.Data.Body}, Repository: scm.RepositoryIdentityFromPRURL(response.Data.URL, "/pulls/", number)}, nil
}

var _ scm.PRRepositoryContentReader = (*Host)(nil)
