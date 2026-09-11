package forgejo

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// forgejo-axi 1.3.0 (source bb2d38c9803dd6ee841ffc84a4627be6b4de6474,
// src/cli.ts handleApi / src/forgejo.ts rawApi) emits {status,data} for
// `api GET PATH --json`. Unlike pr view, this neither previews the body nor
// normalizes an absent body to empty. Existing connection/auth flags still go
// through runJSON. pr update omits absent title all the way to its PATCH.
func (h *Host) GetPRContent(ctx context.Context, pr *scm.PR) (scm.PRContent, error) {
	number, err := h.validateInputPR(pr)
	if err != nil {
		return scm.PRContent{}, err
	}
	var response struct {
		Status int `json:"status"`
		Data   *struct {
			Number int     `json:"number"`
			Title  string  `json:"title"`
			Body   *string `json:"body"`
		} `json:"data"`
	}
	endpoint := "repos/" + h.repository + "/pulls/" + number
	if err := h.runJSON(ctx, "api", []string{"GET", endpoint}, &response); err != nil {
		return scm.PRContent{}, err
	}
	if response.Status != 200 || response.Data == nil || strings.TrimSpace(response.Data.Title) == "" || response.Data.Body == nil {
		return scm.PRContent{}, fmt.Errorf("forgejo-axi api: incomplete raw pull content (requires raw api support)")
	}
	if err := h.validateOutputPRNumber(number, response.Data.Number); err != nil {
		return scm.PRContent{}, err
	}
	return scm.PRContent{Title: response.Data.Title, Body: *response.Data.Body}, nil
}

var _ scm.PRContentReader = (*Host)(nil)
