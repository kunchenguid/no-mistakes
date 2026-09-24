package config

import (
	"fmt"
	"net/url"
	"strings"
)

func normalizeRepositoryOverrides(raw RepositoryOverrides) (RepositoryOverrides, error) {
	overrides := make(RepositoryOverrides, len(raw))
	for remote, override := range raw {
		key, err := normalizeRepositoryRemote(remote)
		if err != nil {
			return nil, fmt.Errorf("invalid repository_overrides remote: %w", err)
		}
		if _, exists := overrides[key]; exists {
			return nil, fmt.Errorf("invalid repository_overrides: duplicate remote after normalization")
		}
		if err := validateGlobalCommitRaw(override.Commit); err != nil {
			return nil, fmt.Errorf("invalid repository_overrides.%s.commit: %w", key, err)
		}
		if override.PR.TitleFormat != nil {
			if err := validatePRTitleFormat(*override.PR.TitleFormat); err != nil {
				return nil, fmt.Errorf("invalid repository_overrides.%s.pr: %w", key, err)
			}
		}
		overrides[key] = override
	}
	return overrides, nil
}

// normalizeRepositoryRemote identifies a Git remote by host and its
// owner/repository path, independent of transport, username, case, or .git.
func normalizeRepositoryRemote(remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", fmt.Errorf("remote must not be empty")
	}

	var host, path string
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil {
			return "", fmt.Errorf("remote URL is invalid")
		}
		scheme := strings.ToLower(parsed.Scheme)
		switch scheme {
		case "https", "http", "ssh", "git":
		default:
			return "", fmt.Errorf("unsupported remote scheme %q", parsed.Scheme)
		}
		if parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", fmt.Errorf("remote URL must not contain opaque data, query, or fragment")
		}
		host = strings.ToLower(parsed.Hostname())
		if port := parsed.Port(); port != "" && !isDefaultRemotePort(scheme, port) {
			host += ":" + port
		}
		path = parsed.EscapedPath()
		path = strings.TrimPrefix(path, "/")
		path, err = url.PathUnescape(path)
		if err != nil {
			return "", fmt.Errorf("decode remote path: %w", err)
		}
	} else {
		// Git's scp-like SSH form is [user@]host:owner/repository.git.
		hostPart, remotePath, found := strings.Cut(remote, ":")
		if !found || strings.Contains(hostPart, "/") {
			return "", fmt.Errorf("expected an HTTPS, HTTP, SSH, or scp-like Git remote")
		}
		if at := strings.LastIndexByte(hostPart, '@'); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		host = strings.ToLower(hostPart)
		path = remotePath
	}

	if host == "" || strings.ContainsAny(host, " \t\r\n/@") {
		return "", fmt.Errorf("remote host is missing or invalid")
	}
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		return "", fmt.Errorf("remote path must contain exactly owner/repository")
	}
	owner := strings.ToLower(parts[0])
	repository := strings.TrimSuffix(strings.ToLower(parts[1]), ".git")
	if !validRemotePathPart(owner) || !validRemotePathPart(repository) {
		return "", fmt.Errorf("remote path must contain non-empty owner and repository")
	}
	return host + "/" + owner + "/" + repository, nil
}

func isDefaultRemotePort(scheme, port string) bool {
	switch scheme {
	case "http":
		return port == "80"
	case "https":
		return port == "443"
	case "ssh":
		return port == "22"
	default:
		return false
	}
}

func validRemotePathPart(part string) bool {
	if part == "" || part == "." || part == ".." || strings.Contains(part, "\\") {
		return false
	}
	return strings.IndexFunc(part, func(r rune) bool { return r <= ' ' || r == 0x7f }) < 0
}
