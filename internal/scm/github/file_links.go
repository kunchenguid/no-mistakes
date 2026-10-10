package github

import (
	"net/url"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

var _ scm.RepositoryFileLinker = (*Host)(nil)

// RepositoryFileLinks returns the github.com blob and raw.githubusercontent.com
// prefixes evidence links have always used; other GitHub hosts get no links.
// remoteURL may name a fork rather than the repository the PR targets.
func (h *Host) RepositoryFileLinks(remoteURL string) (string, string, bool) {
	if h == nil {
		return "", "", false
	}
	webURL, ok := scm.RepositoryWebURL(remoteURL, h.host)
	if !ok {
		return "", "", false
	}
	web, err := url.Parse(webURL)
	if err != nil || !strings.EqualFold(web.Host, "github.com") {
		return "", "", false
	}
	repo := strings.TrimPrefix(web.Path, "/")
	if len(strings.Split(repo, "/")) != 2 || strings.ContainsAny(repo, "\r\n <>[]()\\") || strings.Contains(repo, "..") {
		return "", "", false
	}
	escaped := strings.TrimPrefix(web.EscapedPath(), "/")
	return "https://github.com/" + escaped + "/blob/", "https://raw.githubusercontent.com/" + escaped + "/", true
}
