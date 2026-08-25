package eval

import (
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// A case deliberately stores no remote URL: it identifies its repository only
// by the fingerprint of the redacted upstream URL (see fingerprint). That keeps
// the corpus portable, but it also means a dashboard has nothing human-readable
// to show. RepoDisplayNames rebuilds the mapping locally by fingerprinting the
// repositories already registered in the pipeline database, so an existing case
// resolves without recapture and without any network lookup. A repository the
// database no longer knows simply stays unresolved and keeps its fingerprint.
func RepoDisplayNames(repos []*db.Repo) map[string]string {
	names := map[string]string{}
	for _, repo := range repos {
		if repo == nil {
			continue
		}
		// A repository with no upstream URL was never the source of a case:
		// every fingerprint comes from one. Skipping it also keeps repositories
		// from colliding on the fingerprint of the empty string.
		if strings.TrimSpace(repo.UpstreamURL) == "" {
			continue
		}
		name := RepoDisplayName(repo)
		if name == "" {
			continue
		}
		names[fingerprint(repo.UpstreamURL)] = name
	}
	return names
}

// RepoDisplayName is the human-readable identity of one registered repository:
// its "owner/name" slug when the upstream URL carries one, else the working
// clone's directory name (the same short-name convention the stats dashboard
// uses), else its opaque id.
func RepoDisplayName(repo *db.Repo) string {
	if repo == nil {
		return ""
	}
	if path := scm.RepoPath(repo.UpstreamURL); strings.Contains(path, "/") {
		return path
	}
	if path := strings.TrimSpace(repo.WorkingPath); path != "" {
		base := filepath.Base(path)
		if base != "." && base != string(filepath.Separator) && base != "" {
			return base
		}
		return path
	}
	return strings.TrimSpace(repo.ID)
}
