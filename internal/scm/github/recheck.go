package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// GetChecks already pins both the rollup and workflow-run union to one live
// head and checks it again after discovery. A recheck additionally refuses a
// live head that differs from the one this pipeline delivered.
func (h *Host) GetChecksForHead(ctx context.Context, pr *scm.PR, expectedHead string) ([]scm.Check, error) {
	if strings.TrimSpace(expectedHead) == "" {
		return nil, fmt.Errorf("missing expected CI head")
	}
	pinned := *pr
	pinned.HeadSHA = expectedHead
	checks, err := h.GetChecks(ctx, &pinned)
	if err != nil {
		return nil, err
	}
	if pinned.HeadSHA != expectedHead {
		return nil, fmt.Errorf("%w: expected %s, got %s", scm.ErrHeadChanged, expectedHead, pinned.HeadSHA)
	}
	return checks, nil
}
