package gate

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
)

// RefreshFailureReason is a bounded, URL-free reason safe to emit in logs.
type RefreshFailureReason string

const (
	RefreshRemoteUnreadable RefreshFailureReason = "remote_unreadable"
	RefreshAmbiguousRemote  RefreshFailureReason = "ambiguous_remote"
	RefreshInvalidRemote    RefreshFailureReason = "invalid_remote"
	RefreshDatabaseWrite    RefreshFailureReason = "database_write"
)

type repoURLRefreshError struct {
	reason RefreshFailureReason
}

func (e *repoURLRefreshError) Error() string {
	return "repository URL refresh failed: " + string(e.reason)
}

// ReasonForRefreshFailure returns a bounded reason that contains no remote URL
// or wrapped dependency output.
func ReasonForRefreshFailure(err error) RefreshFailureReason {
	if refreshErr, ok := err.(*repoURLRefreshError); ok {
		return refreshErr.reason
	}
	return RefreshRemoteUnreadable
}

func refreshFailure(reason RefreshFailureReason) error {
	return &repoURLRefreshError{reason: reason}
}

// RefreshRepoURLs best-effort discovers the current registered targets from
// the working clone and atomically replaces both URL fields. Origin remains the
// authoritative upstream source. An existing fork is refreshed only when one
// unique clone remote still identifies that same fork repository.
//
// It never mutates Git configuration. Every failure is deliberately URL-free
// so callers can safely log only ReasonForRefreshFailure and continue with the
// exact repo value they already hold.
func RefreshRepoURLs(ctx context.Context, database *db.DB, repo *db.Repo) (*db.Repo, bool, error) {
	if database == nil || repo == nil || strings.TrimSpace(repo.WorkingPath) == "" {
		return nil, false, refreshFailure(RefreshRemoteUnreadable)
	}

	originURLs, err := git.GetConfiguredRemoteURLs(ctx, repo.WorkingPath, "origin")
	if err != nil || len(originURLs) == 0 {
		return nil, false, refreshFailure(RefreshRemoteUnreadable)
	}
	if len(originURLs) != 1 {
		return nil, false, refreshFailure(RefreshAmbiguousRemote)
	}
	upstreamURL := originURLs[0]
	upstream, err := inspectRefreshRemote(upstreamURL)
	if err != nil {
		return nil, false, refreshFailure(RefreshInvalidRemote)
	}

	forkURL := ""
	if strings.TrimSpace(repo.ForkURL) != "" {
		registeredFork, oldErr := inspectRefreshRemote(repo.ForkURL)
		if oldErr != nil {
			return nil, false, refreshFailure(RefreshInvalidRemote)
		}
		remoteNames, listErr := git.Run(ctx, repo.WorkingPath, "remote")
		if listErr != nil {
			return nil, false, refreshFailure(RefreshRemoteUnreadable)
		}
		type forkCandidate struct {
			name string
			url  string
		}
		var candidates []forkCandidate
		for _, name := range strings.Fields(remoteNames) {
			if name == "origin" || name == RemoteName {
				continue
			}
			candidateURLs, readErr := git.GetConfiguredRemoteURLs(ctx, repo.WorkingPath, name)
			if readErr != nil || len(candidateURLs) == 0 {
				return nil, false, refreshFailure(RefreshRemoteUnreadable)
			}
			if len(candidateURLs) != 1 {
				return nil, false, refreshFailure(RefreshAmbiguousRemote)
			}
			candidateURL := candidateURLs[0]
			candidate, candidateErr := inspectRefreshRemote(candidateURL)
			if candidateErr != nil {
				if name == "fork" || candidate.identity == registeredFork.identity {
					return nil, false, refreshFailure(RefreshInvalidRemote)
				}
				continue
			}
			if candidate.identity == registeredFork.identity {
				candidates = append(candidates, forkCandidate{name: name, url: candidate.raw})
			}
		}
		if len(candidates) == 0 {
			return nil, false, refreshFailure(RefreshRemoteUnreadable)
		}
		if len(candidates) != 1 {
			return nil, false, refreshFailure(RefreshAmbiguousRemote)
		}
		forkURL = candidates[0].url
		// Re-read the selected fork source immediately before replacement. A
		// concurrent git-config edit makes the source ambiguous rather than
		// allowing a mixed snapshot into the registry.
		confirmed, confirmErr := git.GetConfiguredRemoteURLs(ctx, repo.WorkingPath, candidates[0].name)
		if confirmErr != nil || len(confirmed) != 1 || confirmed[0] != forkURL {
			return nil, false, refreshFailure(RefreshAmbiguousRemote)
		}
	}

	// Re-read origin at the replacement boundary for the same reason. This is
	// still read-only and does not alter clone configuration.
	confirmedOrigin, confirmErr := git.GetConfiguredRemoteURLs(ctx, repo.WorkingPath, "origin")
	if confirmErr != nil || len(confirmedOrigin) != 1 || confirmedOrigin[0] != upstream.raw {
		return nil, false, refreshFailure(RefreshAmbiguousRemote)
	}

	if forkURL != "" {
		if err := validateForkRouting(ctx, upstream.raw, forkURL); err != nil {
			return nil, false, refreshFailure(RefreshInvalidRemote)
		}
	}

	if upstream.raw == repo.UpstreamURL && forkURL == repo.ForkURL {
		verified := *repo
		verified.URLsVerified = true
		return &verified, false, nil
	}
	updated, err := database.ReplaceRepoURLs(repo.ID, upstream.raw, forkURL)
	if err != nil {
		return nil, false, refreshFailure(RefreshDatabaseWrite)
	}
	updated.URLsVerified = true
	return updated, true, nil
}

type refreshRemote struct {
	raw      string
	identity string
}

func SameRemoteRepository(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	if safeurl.Redact(left) == safeurl.Redact(right) {
		return true
	}
	leftRemote, _ := inspectRefreshRemote(left)
	rightRemote, _ := inspectRefreshRemote(right)
	return leftRemote.identity != "" && leftRemote.identity == rightRemote.identity
}

func inspectRefreshRemote(raw string) (refreshRemote, error) {
	trimmed := strings.TrimSpace(raw)
	info := refreshRemote{raw: trimmed}
	if trimmed == "" || trimmed != raw || strings.IndexFunc(trimmed, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return info, fmt.Errorf("invalid remote")
	}

	var remotePath string
	if strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return info, fmt.Errorf("invalid remote")
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https":
			if parsed.User != nil {
				remotePath = parsed.Path
				info.identity = refreshRemoteIdentity(trimmed, remotePath)
				return info, fmt.Errorf("credential-bearing remote")
			}
		case "ssh", "git":
			if parsed.User != nil {
				if _, hasPassword := parsed.User.Password(); hasPassword {
					remotePath = parsed.Path
					info.identity = refreshRemoteIdentity(trimmed, remotePath)
					return info, fmt.Errorf("credential-bearing remote")
				}
			}
		default:
			return info, fmt.Errorf("unsupported remote")
		}
		remotePath = parsed.Path
	} else {
		colon := strings.IndexByte(trimmed, ':')
		hostSpec := ""
		if colon > 0 {
			hostSpec = trimmed[:colon]
		}
		if colon <= 0 || colon == len(trimmed)-1 || strings.ContainsAny(hostSpec, `/\\?#`) || strings.ContainsAny(trimmed[colon+1:], "?#") || strings.Count(hostSpec, "@") > 1 {
			return info, fmt.Errorf("invalid remote")
		}
		if at := strings.IndexByte(hostSpec, '@'); at >= 0 && (at == 0 || at == len(hostSpec)-1) {
			return info, fmt.Errorf("invalid remote")
		}
		remotePath = trimmed[colon+1:]
	}

	info.identity = refreshRemoteIdentity(trimmed, remotePath)
	if info.identity == "" {
		return info, fmt.Errorf("partial remote")
	}
	return info, nil
}

func refreshRemoteIdentity(raw, remotePath string) string {
	authority := ""
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		authority = strings.ToLower(parsed.Hostname())
		if port := parsed.Port(); port != "" {
			authority += ":" + port
		}
	} else if colon := strings.IndexByte(raw, ':'); colon > 0 {
		authority = raw[:colon]
		if at := strings.LastIndexByte(authority, '@'); at >= 0 {
			authority = authority[at+1:]
		}
		authority = strings.ToLower(authority)
	}
	path := strings.Trim(strings.TrimSpace(remotePath), "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if authority == "" || len(parts) < 2 {
		return ""
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" || part == "." || part == ".." {
			return ""
		}
	}
	return authority + "/" + path
}
