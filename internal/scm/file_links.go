package scm

import (
	"net"
	"net/url"
	"strings"
)

// RepositoryWebURL turns a registered Git remote into the project's web URL,
// without credentials, query, or fragment. An HTTP(S) remote keeps its scheme
// and port. SSH and git:// remotes are linked over HTTPS on the default port,
// and their transport port is dropped. resolvedHost is the run's resolved
// forge host, so an SSH alias maps to the real host. Every namespace segment
// is kept and escaped on its own.
func RepositoryWebURL(remoteURL, resolvedHost string) (string, bool) {
	remoteURL = strings.TrimSpace(remoteURL)
	scheme, port := "https", ""
	if strings.Contains(remoteURL, "://") {
		parsed, err := url.Parse(remoteURL)
		if err != nil || parsed.Host == "" {
			return "", false
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https":
			scheme, port = strings.ToLower(parsed.Scheme), parsed.Port()
		case "ssh", "git":
		default:
			return "", false
		}
	} else {
		colon := strings.IndexByte(remoteURL, ':')
		if colon <= 0 || strings.ContainsAny(remoteURL[:colon], "/\\") {
			return "", false
		}
	}
	host := strings.TrimSpace(resolvedHost)
	if host == "" {
		host = ExtractHost(remoteURL)
	}
	if host == "" || strings.ContainsAny(host, " \t\r\n/\\?#@<>") {
		return "", false
	}
	if port != "" {
		host = net.JoinHostPort(strings.Trim(host, "[]"), port)
	}
	parts := strings.Split(RepoPath(remoteURL), "/")
	if len(parts) < 2 {
		return "", false
	}
	for i, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\r\n\t\\<>") {
			return "", false
		}
		parts[i] = url.PathEscape(part)
	}
	web, err := url.Parse(scheme + "://" + host + "/" + strings.Join(parts, "/"))
	if err != nil || web.Hostname() == "" || web.User != nil {
		return "", false
	}
	return web.String(), true
}
