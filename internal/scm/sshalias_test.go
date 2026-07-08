package scm

import "testing"

// stubResolver installs a deterministic ssh-alias resolver for the duration of
// the test, so unit tests never shell out to a real ssh. aliases maps an alias
// to its resolved HostName; hosts absent from the map resolve to themselves
// (mirroring `ssh -G` echoing a non-aliased host back).
func stubResolver(t *testing.T, aliases map[string]string) {
	t.Helper()
	prev := sshHostAliasResolver
	t.Cleanup(func() { sshHostAliasResolver = prev })
	sshHostAliasResolver = func(host string) (string, bool) {
		if resolved, ok := aliases[host]; ok {
			return resolved, true
		}
		return host, true
	}
}

func TestResolveHostAlias(t *testing.T) {
	stubResolver(t, map[string]string{"github-acme": "github.com"})

	if got := ResolveHostAlias("github-acme"); got != "github.com" {
		t.Fatalf("ResolveHostAlias(github-acme) = %q, want github.com", got)
	}
	if got := ResolveHostAlias("github.com"); got != "github.com" {
		t.Fatalf("ResolveHostAlias(github.com) = %q, want unchanged", got)
	}
	if got := ResolveHostAlias(""); got != "" {
		t.Fatalf("ResolveHostAlias(\"\") = %q, want empty", got)
	}
}

func TestResolveHostAliasFailsClosed(t *testing.T) {
	prev := sshHostAliasResolver
	t.Cleanup(func() { sshHostAliasResolver = prev })
	sshHostAliasResolver = func(string) (string, bool) { return "", false } // ssh unavailable

	if got := ResolveHostAlias("github-acme"); got != "github-acme" {
		t.Fatalf("ResolveHostAlias with unavailable ssh = %q, want input unchanged", got)
	}
}

func TestCanonicalRemoteURL(t *testing.T) {
	stubResolver(t, map[string]string{
		"github-acme": "github.com",
		"gl-work":     "gitlab.com",
	})

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"scp alias", "git@github-acme:acme/app.git", "git@github.com:acme/app.git"},
		{"scp alias no user", "github-acme:acme/app.git", "github.com:acme/app.git"},
		{"ssh url alias with port", "ssh://git@gl-work:22/grp/proj.git", "ssh://git@gitlab.com:22/grp/proj.git"},
		{"ssh url alias no port", "ssh://git@gl-work/grp/proj.git", "ssh://git@gitlab.com/grp/proj.git"},
		{"scp non-alias unchanged", "git@github.com:acme/app.git", "git@github.com:acme/app.git"},
		{"https never rewritten", "https://github-acme/acme/app.git", "https://github-acme/acme/app.git"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalRemoteURL(tc.in); got != tc.want {
				t.Fatalf("CanonicalRemoteURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRemoteUsesSSH(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"git@github.com:acme/app.git", true},
		{"ssh://git@github.com/acme/app.git", true},
		{"https://github.com/acme/app.git", false},
		{"http://github.com/acme/app.git", false},
		{"/local/path", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := remoteUsesSSH(tc.in); got != tc.want {
			t.Fatalf("remoteUsesSSH(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestDetectProvider_SSHAlias is the regression for issue #290: an ssh-alias
// remote must classify to its real provider instead of ProviderUnknown, so the
// pr/ci steps run instead of silently skipping.
func TestDetectProvider_SSHAlias(t *testing.T) {
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	stubResolver(t, map[string]string{
		"github-acme": "github.com",
		"gl-work":     "gitlab.com",
	})

	cases := []struct {
		name string
		in   string
		want Provider
	}{
		{"github ssh alias scp", "git@github-acme:acme/app.git", ProviderGitHub},
		{"gitlab ssh alias url", "ssh://git@gl-work/grp/proj.git", ProviderGitLab},
		{"unknown alias stays unknown", "git@some-random-alias:acme/app.git", ProviderUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectProvider(tc.in); got != tc.want {
				t.Fatalf("DetectProvider(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDetectProvider_LiteralHostSkipsAliasResolution guards the hot path: a
// literal github.com/gitlab.com remote must classify without invoking the ssh
// resolver at all.
func TestDetectProvider_LiteralHostSkipsAliasResolution(t *testing.T) {
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	prev := sshHostAliasResolver
	t.Cleanup(func() { sshHostAliasResolver = prev })
	sshHostAliasResolver = func(host string) (string, bool) {
		t.Fatalf("ssh resolver must not run for a literal host, got %q", host)
		return "", false
	}

	if got := DetectProvider("git@github.com:acme/app.git"); got != ProviderGitHub {
		t.Fatalf("DetectProvider(github.com) = %q, want github", got)
	}
	// https remote with an unknown host must not trigger ssh resolution either.
	if got := DetectProvider("https://unknown-host.example/acme/app.git"); got != ProviderUnknown {
		t.Fatalf("DetectProvider(https unknown) = %q, want unknown", got)
	}
}
