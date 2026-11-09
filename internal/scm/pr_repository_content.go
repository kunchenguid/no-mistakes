package scm

import (
	"context"
	"net"
	"net/url"
	"strings"
)

// RepositoryIdentity identifies the destination repository reported by a read,
// independently of the number used to request the PR. Path includes any forge
// base path or project namespace. Host includes a non-default web port so two
// forge instances on one hostname do not acquire each other's identity.
type RepositoryIdentity struct {
	Host string
	Path string
}

type PRRepositoryContent struct {
	Content    PRContent
	Repository RepositoryIdentity
}

// PRRepositoryContentReader is separate from publication's PRContentReader:
// an unbound numbered read needs positive repository identity as well as text.
type PRRepositoryContentReader interface {
	GetPRRepositoryContent(context.Context, string) (PRRepositoryContent, error)
}

func RepositoryIdentityFromURL(raw string) RepositoryIdentity {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return RepositoryIdentity{}
	}
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" && !(u.Scheme == "https" && port == "443") && !(u.Scheme == "http" && port == "80") {
		host = net.JoinHostPort(host, port)
	}
	return RepositoryIdentity{Host: host, Path: strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")}
}

// RepositoryIdentityFromPRURL accepts only the provider's exact numbered web
// route. Missing or unrecognizable evidence stays empty for the caller to refuse.
func RepositoryIdentityFromPRURL(raw, route, number string) RepositoryIdentity {
	if raw == "" || number == "" || !strings.HasSuffix(raw, route+number) {
		return RepositoryIdentity{}
	}
	return RepositoryIdentityFromURL(strings.TrimSuffix(raw, route+number))
}
