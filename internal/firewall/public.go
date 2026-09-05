package firewall

import (
	"net/url"
	"path"
	"strings"
)

// PublicSummary is the only text allowed in GitHub check output.
type PublicSummary struct {
	Phrase    string `json:"phrase"`
	PortalURL string `json:"portal_url,omitempty"`
}

// Notice is the only payload a Discord (or other off-LAN) notifier may send.
// It contains repository and PR URLs that are already public, a portal URL,
// and a conclusion. It never contains snippets, filenames, titles, or names.
type Notice struct {
	Kind       string `json:"kind"`
	Repo       string `json:"repo,omitempty"`
	PRURL      string `json:"pr_url,omitempty"`
	PortalURL  string `json:"portal_url,omitempty"`
	Conclusion string `json:"conclusion"`
}

func PublicText(portalURL string, failed bool) string {
	if !failed {
		return "publish-policy ok\n"
	}
	var b strings.Builder
	b.WriteString(PublicPhrase)
	b.WriteByte('\n')
	if u := strings.TrimSpace(portalURL); u != "" {
		b.WriteString(u)
		b.WriteByte('\n')
	}
	return b.String()
}

func PortalPath(id string) string {
	return "/v1/axi/runs/" + id
}

func JoinPortalURL(base, id string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return strings.TrimRight(base, "/") + PortalPath(id)
	}
	u.Path = path.Join(strings.TrimSuffix(u.Path, "/"), PortalPath(id))
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func NewNotice(repo, prURL, portalURL, conclusion string) Notice {
	kind := "publish-policy-ok"
	if conclusion != "success" {
		kind = "publish-policy-violation"
	}
	return Notice{
		Kind:       kind,
		Repo:       repo,
		PRURL:      prURL,
		PortalURL:  portalURL,
		Conclusion: conclusion,
	}
}

// ContainsForbiddenPublic reports whether s includes a match snippet from
// findings. Used by tests to prove GitHub/notice output stays generic.
func ContainsForbiddenPublic(s string, findings []Finding) bool {
	lower := strings.ToLower(s)
	if strings.Contains(lower, "@") && strings.Contains(lower, ".") {
		// Email-shaped text is never allowed on a public surface.
		if emailCandidate.FindString(s) != "" {
			return true
		}
	}
	for _, f := range findings {
		if f.File != "" && strings.Contains(s, f.File) {
			return true
		}
		if f.Description != "" {
			// Description is LAN-private; any copy into public text is a leak.
			snip := f.Description
			if i := strings.LastIndex(snip, ": "); i >= 0 {
				snip = snip[i+2:]
			}
			if snip != "" && strings.Contains(s, snip) {
				return true
			}
		}
	}
	return ipv4Candidate.FindString(s) != "" || macCandidate.FindString(s) != ""
}
