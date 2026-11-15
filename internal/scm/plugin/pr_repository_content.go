package plugin

import (
	"context"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func (h *Host) GetPRRepositoryContent(ctx context.Context, number string) (scm.PRRepositoryContent, error) {
	result, _, err := h.viewPR(ctx, &scm.PR{Number: number})
	if err != nil {
		return scm.PRRepositoryContent{}, err
	}
	if result.Title == nil || result.Body == nil {
		return scm.PRRepositoryContent{}, h.protocolf(CmdPRView, "response must include both title and body")
	}
	// viewPR's normalizePR/samePR prove the returned URL contains this exact
	// repository path, refusing absent or foreign identity as ErrProtocol.
	// The protocol intentionally permits a different web host than the git
	// host, so name the transport-bound repository rather than that web alias.
	identity := scm.RepositoryIdentity{Host: h.opts.Repository.Host, Path: h.opts.Repository.Path}
	return scm.PRRepositoryContent{Content: scm.PRContent{Title: *result.Title, Body: *result.Body}, Repository: identity}, nil
}

var _ scm.PRRepositoryContentReader = (*Host)(nil)
