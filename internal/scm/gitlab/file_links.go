package gitlab

import (
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

var _ scm.RepositoryFileLinker = (*Host)(nil)

// RepositoryFileLinks returns the project's /-/blob/ and /-/raw/ prefixes, for
// nested subgroups and self-hosted instances alike. The links carry no token;
// for a private project the viewer's own GitLab session decides access.
func (h *Host) RepositoryFileLinks(remoteURL string) (string, string, bool) {
	if h == nil || h.projectPath == "" || scm.RepoPath(remoteURL) != h.projectPath {
		return "", "", false
	}
	webURL, ok := scm.RepositoryWebURL(remoteURL, h.host)
	if !ok {
		return "", "", false
	}
	return webURL + "/-/blob/", webURL + "/-/raw/", true
}
