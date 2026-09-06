package firewall

import (
	"net/url"
	"path"
	"strings"
)

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
	return publicMessage(portalURL, PublicPhrase)
}

func PublicErrorText(portalURL string) string {
	return publicMessage(portalURL, "publish-policy error")
}

func publicMessage(portalURL, phrase string) string {
	var b strings.Builder
	b.WriteString(phrase)
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
	if conclusion == "error" {
		kind = "publish-policy-error"
	} else if conclusion != "success" {
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
