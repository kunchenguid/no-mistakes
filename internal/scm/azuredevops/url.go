package azuredevops

import (
	"net/url"
	"strings"
)

// ParseRemote extracts the Azure DevOps organization URL, project name, and
// repository name from a git remote (or pull-request) URL. It returns
// ok=false for any non-Azure remote or when a component cannot be determined.
//
// Supported forms:
//
//	https://dev.azure.com/{org}/{project}/_git/{repo}
//	https://{org}@dev.azure.com/{org}/{project}/_git/{repo}
//	git@ssh.dev.azure.com:v3/{org}/{project}/{repo}
//	https://{org}.visualstudio.com/[{collection}/]{project}/_git/{repo}
//	git@vs-ssh.visualstudio.com:v3/{org}/{project}/{repo}
//
// Any of the above may carry a trailing /pullrequest/{id} segment (a PR URL),
// which is ignored. Project names may contain spaces; path segments are
// percent-decoded. The returned orgURL is a fully-qualified organization URL
// suitable for the az CLI's --organization flag (e.g.
// https://dev.azure.com/myorg).
func ParseRemote(remote string) (orgURL, project, repo string, ok bool) {
	s := strings.TrimSpace(remote)
	if s == "" {
		return "", "", "", false
	}
	s = strings.TrimSuffix(s, ".git")

	var host string
	var segments []string

	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", "", "", false
		}
		host = strings.ToLower(u.Hostname())
		segments = splitDecodePath(u.EscapedPath())
	} else {
		// scp-like syntax: [user@]host:path. The first ':' separates host from
		// path; bail when a '/' precedes it (not scp form).
		c := strings.Index(s, ":")
		if c < 0 || strings.Contains(s[:c], "/") {
			return "", "", "", false
		}
		hostPart := s[:c]
		if at := strings.LastIndex(hostPart, "@"); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		host = strings.ToLower(hostPart)
		segments = splitDecodePath(s[c+1:])
	}

	isVisualStudio := strings.HasSuffix(host, ".visualstudio.com")
	if host != "dev.azure.com" && host != "ssh.dev.azure.com" && !isVisualStudio {
		return "", "", "", false
	}

	// Azure SSH paths are prefixed with a literal "v3" segment.
	if len(segments) > 0 && strings.EqualFold(segments[0], "v3") {
		segments = segments[1:]
	}
	if len(segments) == 0 {
		return "", "", "", false
	}

	// HTTPS forms carry a "_git" marker; SSH forms do not. Locating the repo by
	// the marker tolerates an optional collection segment and a /pullrequest
	// suffix.
	gitIdx := -1
	for i, seg := range segments {
		if seg == "_git" {
			gitIdx = i
			break
		}
	}

	var org string
	if gitIdx >= 0 {
		if gitIdx == 0 || gitIdx+1 >= len(segments) {
			return "", "", "", false
		}
		project = segments[gitIdx-1]
		repo = segments[gitIdx+1]
		if isVisualStudio {
			org = strings.TrimSuffix(host, ".visualstudio.com")
		} else {
			org = segments[0]
		}
	} else {
		// SSH form: {org}/{project}/{repo}
		if len(segments) < 3 {
			return "", "", "", false
		}
		org, project, repo = segments[0], segments[1], segments[2]
	}

	org = strings.TrimSpace(org)
	project = strings.TrimSpace(project)
	repo = strings.TrimSpace(repo)
	if org == "" || project == "" || repo == "" {
		return "", "", "", false
	}

	if isVisualStudio {
		orgURL = "https://" + org + ".visualstudio.com"
	} else {
		orgURL = "https://dev.azure.com/" + org
	}
	return orgURL, project, repo, true
}

func splitDecodePath(p string) []string {
	raw := strings.Split(strings.Trim(p, "/"), "/")
	out := make([]string, 0, len(raw))
	for _, seg := range raw {
		if seg == "" {
			continue
		}
		if dec, err := url.PathUnescape(seg); err == nil {
			out = append(out, dec)
		} else {
			out = append(out, seg)
		}
	}
	return out
}

// webPRURL builds the browsable pull-request URL. It prefers the repository web
// URL returned by the API; otherwise it constructs one from the org URL,
// project, and repo (percent-encoding the project and repo segments). The az
// CLI returns an _apis/... endpoint in the PR's top-level "url" field, which is
// not browsable, so the human URL must be built rather than read from there.
func webPRURL(orgURL, project, repo, repoWebURL, id string) string {
	base := strings.TrimRight(strings.TrimSpace(repoWebURL), "/")
	if base == "" {
		base = strings.TrimRight(orgURL, "/") + "/" + url.PathEscape(project) + "/_git/" + url.PathEscape(repo)
	}
	if id == "" {
		return base
	}
	return base + "/pullrequest/" + id
}
