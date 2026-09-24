package config

import (
	"fmt"
	"net"
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

// normalizeRepositoryRemote identifies a Git remote by host and repository path,
// applying provider-specific case and suffix equivalence.
func normalizeRepositoryRemote(remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", fmt.Errorf("remote must not be empty")
	}

	var host, portSuffix, rawPath, scheme string
	escapedPath := false
	absolutePath := false
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil {
			return "", fmt.Errorf("remote URL is invalid")
		}
		scheme = strings.ToLower(parsed.Scheme)
		switch scheme {
		case "https", "http", "ssh", "git":
		default:
			return "", fmt.Errorf("unsupported remote scheme %q", parsed.Scheme)
		}
		if parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", fmt.Errorf("remote URL must not contain opaque data, query, or fragment")
		}
		host = parsed.Hostname()
		if port := parsed.Port(); port != "" && !isDefaultRemotePort(scheme, port) {
			portSuffix = ":" + port
		}
		rawPath = parsed.EscapedPath()
		escapedPath = true
		absolutePath = scheme == "ssh" && strings.HasPrefix(rawPath, "/")
	} else {
		var err error
		host, rawPath, err = parseSCPRemote(remote)
		if err != nil {
			return "", err
		}
		absolutePath = strings.HasPrefix(rawPath, "/")
	}

	if ip := net.ParseIP(host); ip != nil && strings.Contains(host, ":") {
		host = ip.String()
	} else {
		host = strings.ToLower(host)
	}
	if host == "" || strings.ContainsAny(host, " \t\r\n/@") {
		return "", fmt.Errorf("remote host is missing or invalid")
	}
	parts, err := normalizeRemotePath(rawPath, escapedPath, caseInsensitiveRepositoryPathHost(host))
	if err != nil {
		return "", err
	}
	if len(parts) < 2 {
		return "", fmt.Errorf("remote path must contain at least owner/repository")
	}
	pathPrefix := "/"
	if absolutePath && !repositoryNamespaceHost(host) {
		pathPrefix = "//"
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host + portSuffix + pathPrefix + strings.Join(parts, "/"), nil
}

func parseSCPRemote(remote string) (string, string, error) {
	firstColon := strings.IndexByte(remote, ':')
	openBracket := strings.IndexByte(remote, '[')
	if openBracket >= 0 && (firstColon < 0 || openBracket < firstColon) {
		closeRelative := strings.IndexByte(remote[openBracket+1:], ']')
		if closeRelative < 0 {
			return "", "", fmt.Errorf("expected an HTTPS, HTTP, SSH, or scp-like Git remote")
		}
		closeBracket := openBracket + 1 + closeRelative
		if closeBracket+1 >= len(remote) || remote[closeBracket+1] != ':' {
			return "", "", fmt.Errorf("expected an HTTPS, HTTP, SSH, or scp-like Git remote")
		}
		hostPart := remote[:closeBracket+1]
		if strings.Contains(hostPart, "/") {
			return "", "", fmt.Errorf("expected an HTTPS, HTTP, SSH, or scp-like Git remote")
		}
		if at := strings.LastIndexByte(hostPart, '@'); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		if len(hostPart) < 4 || hostPart[0] != '[' || hostPart[len(hostPart)-1] != ']' {
			return "", "", fmt.Errorf("invalid bracketed IPv6 scp host")
		}
		ip := hostPart[1 : len(hostPart)-1]
		if !strings.Contains(ip, ":") || net.ParseIP(ip) == nil {
			return "", "", fmt.Errorf("invalid bracketed IPv6 scp host")
		}
		return ip, remote[closeBracket+2:], nil
	}

	hostPart, remotePath, found := strings.Cut(remote, ":")
	if !found || strings.Contains(hostPart, "/") {
		return "", "", fmt.Errorf("expected an HTTPS, HTTP, SSH, or scp-like Git remote")
	}
	if at := strings.LastIndexByte(hostPart, '@'); at >= 0 {
		hostPart = hostPart[at+1:]
	}
	return hostPart, remotePath, nil
}

func normalizeRemotePath(rawPath string, escaped, caseInsensitive bool) ([]string, error) {
	rawPath = strings.TrimPrefix(rawPath, "/")
	if strings.HasSuffix(rawPath, "/") {
		rawPath = strings.TrimSuffix(rawPath, "/")
	}
	if rawPath == "" {
		return nil, fmt.Errorf("remote path must not be empty")
	}
	rawParts := strings.Split(rawPath, "/")
	parts := make([]string, 0, len(rawParts))
	for _, rawPart := range rawParts {
		if rawPart == "" {
			return nil, fmt.Errorf("remote path must not contain empty segments")
		}
		part := rawPart
		if escaped {
			decoded, err := url.PathUnescape(rawPart)
			if err != nil {
				return nil, fmt.Errorf("decode remote path: %w", err)
			}
			part = decoded
		}
		if !validRemotePathPart(part) {
			return nil, fmt.Errorf("remote path contains an invalid segment")
		}
		if caseInsensitive {
			part = strings.ToLower(part)
		}
		parts = append(parts, part)
	}
	last := len(parts) - 1
	if caseInsensitive {
		parts[last] = strings.TrimSuffix(parts[last], ".git")
	}
	if !validRemotePathPart(parts[last]) {
		return nil, fmt.Errorf("remote path must end in a repository name")
	}
	return parts, nil
}

func caseInsensitiveRepositoryPathHost(host string) bool {
	switch host {
	case "github.com", "gitlab.com", "bitbucket.org":
		return true
	default:
		return false
	}
}

func repositoryNamespaceHost(host string) bool {
	switch host {
	case "github.com", "gitlab.com", "bitbucket.org":
		return true
	default:
		return false
	}
}

func isDefaultRemotePort(scheme, port string) bool {
	switch scheme {
	case "http":
		return port == "80"
	case "https":
		return port == "443"
	case "ssh":
		return port == "22"
	case "git":
		return port == "9418"
	default:
		return false
	}
}

func validRemotePathPart(part string) bool {
	if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\") {
		return false
	}
	return strings.IndexFunc(part, func(r rune) bool { return r <= ' ' || r == 0x7f }) < 0
}
