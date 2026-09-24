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

// normalizeRepositoryRemote identifies a Git remote by host and its complete
// repository path, independent of transport, username, case, or .git.
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
	parts, err := normalizeRemotePath(rawPath, escapedPath)
	if err != nil {
		return "", err
	}
	if len(parts) < 2 {
		return "", fmt.Errorf("remote path must contain at least owner/repository")
	}
	host, parts, err = normalizeAzureDevOpsRemote(host, parts)
	if err != nil {
		return "", err
	}
	pathPrefix := "/"
	if absolutePath && !repositoryNamespaceHost(host) {
		pathPrefix = "//"
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

func normalizeRemotePath(rawPath string, escaped bool) ([]string, error) {
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
		parts = append(parts, strings.ToLower(part))
	}
	last := len(parts) - 1
	parts[last] = strings.TrimSuffix(parts[last], ".git")
	if !validRemotePathPart(parts[last]) {
		return nil, fmt.Errorf("remote path must end in a repository name")
	}
	return parts, nil
}

// normalizeAzureDevOpsRemote maps Azure DevOps HTTPS and SSH routing forms to
// one host and org/project/repository path. Other providers keep their complete
// nested namespace unchanged.
func normalizeAzureDevOpsRemote(host string, parts []string) (string, []string, error) {
	azure := false
	sshAlias := false
	switch {
	case host == "dev.azure.com":
		azure = true
	case host == "ssh.dev.azure.com", host == "vs-ssh.visualstudio.com":
		azure = true
		sshAlias = true
	case strings.HasSuffix(host, ".visualstudio.com"):
		organization := strings.TrimSuffix(host, ".visualstudio.com")
		if organization != "" {
			parts = append([]string{organization}, parts...)
			azure = true
		}
	}
	if !azure {
		return host, parts, nil
	}

	if sshAlias && len(parts) > 0 && parts[0] == "v3" {
		parts = parts[1:]
	}
	for i, part := range parts {
		if part != "_git" {
			continue
		}
		if i == 0 || i+2 != len(parts) {
			return "", nil, fmt.Errorf("Azure DevOps remote has an invalid _git path")
		}
		parts = append(parts[:i], parts[i+1:]...)
		break
	}
	if len(parts) < 3 {
		return "", nil, fmt.Errorf("Azure DevOps remote path must identify an organization, project, and repository")
	}
	return "dev.azure.com", parts, nil
}

func repositoryNamespaceHost(host string) bool {
	switch {
	case host == "github.com" || strings.HasSuffix(host, ".github.com"):
		return true
	case host == "gitlab.com" || strings.HasSuffix(host, ".gitlab.com"):
		return true
	case host == "bitbucket.org" || strings.HasSuffix(host, ".bitbucket.org"):
		return true
	case host == "dev.azure.com" || strings.HasSuffix(host, ".dev.azure.com"):
		return true
	case strings.HasSuffix(host, ".visualstudio.com"), host == "codeberg.org":
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
