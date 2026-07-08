package scm

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// sshHostAliasResolver resolves an SSH host alias (a `Host` block in
// ~/.ssh/config) to its effective HostName. It returns (resolvedHost, true) on
// success and ("", false) when resolution is unavailable (e.g. ssh missing or
// erroring), so callers fail closed to the unresolved host. It is a package
// variable so tests can substitute a deterministic resolver instead of shelling
// out to a real ssh.
var sshHostAliasResolver = resolveHostAliasViaSSH

// ResolveHostAlias returns the effective hostname for an SSH host alias,
// resolving `Host`/`HostName` entries in the user's ssh config the same way git
// does when it pushes. When the host is not an alias, resolution is
// unavailable, or the result is empty, the input host is returned unchanged so
// behavior never regresses.
func ResolveHostAlias(host string) string {
	h := strings.TrimSpace(host)
	if h == "" {
		return host
	}
	resolved, ok := sshHostAliasResolver(h)
	if !ok {
		return host
	}
	if resolved = strings.TrimSpace(resolved); resolved == "" {
		return host
	}
	return resolved
}

// CanonicalRemoteURL rewrites an SSH remote whose host is an ssh-config alias so
// that the host names the alias's real HostName. Only scp-style
// (git@host:path) and ssh:// remotes are considered — ssh aliases do not apply
// to https/git remotes, which are returned unchanged. Anything that cannot be
// resolved is returned unchanged, so the result is always a usable remote URL.
func CanonicalRemoteURL(remote string) string {
	s := strings.TrimSpace(remote)
	if s == "" || !remoteUsesSSH(s) {
		return remote
	}

	// Locate the authority (host[:port]) region for the two SSH forms.
	var authStart, authEnd int
	if i := strings.Index(s, "://"); i >= 0 {
		authStart = i + len("://")
		authEnd = len(s)
		if slash := strings.IndexByte(s[authStart:], '/'); slash >= 0 {
			authEnd = authStart + slash
		}
	} else {
		colon := strings.IndexByte(s, ':')
		if colon < 0 {
			return remote
		}
		authStart, authEnd = 0, colon
	}

	authority := s[authStart:authEnd]
	// Preserve any userinfo ("git@") prefix; resolve only the host token.
	userinfo := ""
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		userinfo = authority[:at+1]
		authority = authority[at+1:]
	}
	host, port := authority, ""
	if !strings.HasPrefix(host, "[") { // leave IPv6 literals untouched
		if c := strings.LastIndexByte(host, ':'); c >= 0 && isAllDigits(host[c+1:]) {
			host, port = host[:c], host[c:]
		}
	}

	resolved := ResolveHostAlias(host)
	if resolved == "" || strings.EqualFold(resolved, host) {
		return remote
	}
	return s[:authStart] + userinfo + resolved + port + s[authEnd:]
}

// remoteUsesSSH reports whether remote is an ssh:// URL or scp-style
// (git@host:path) remote, the only forms an ssh-config alias applies to.
func remoteUsesSSH(remote string) bool {
	s := strings.TrimSpace(remote)
	if i := strings.Index(s, "://"); i >= 0 {
		return strings.EqualFold(s[:i], "ssh")
	}
	// scp-style requires a ':' host/path separator that comes before any '/'.
	colon := strings.IndexByte(s, ':')
	if colon < 0 {
		return false
	}
	slash := strings.IndexByte(s, '/')
	return slash < 0 || colon < slash
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// resolveHostAliasViaSSH runs `ssh -G <host>` and returns the effective
// HostName it reports. ssh echoes the input back for a non-aliased host, so an
// unchanged result is indistinguishable from "no alias" — which is exactly the
// fail-closed behavior callers want. Any error (ssh absent, config error,
// timeout) reports (\"\", false).
func resolveHostAliasViaSSH(host string) (string, bool) {
	if _, err := exec.LookPath("ssh"); err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-G", host)
	winproc.Harden(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], "hostname") {
			return fields[1], true
		}
	}
	return "", false
}
