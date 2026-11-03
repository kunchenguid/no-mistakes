package scm

import (
	"context"
	"strings"
	"testing"
)

const (
	sshDomainBase   = "https://forge.example:3443/git"
	sshDomainHost   = "ssh.forge.example"
	sshDomainRemote = "git@ssh.forge.example:git/octo/widgets.git"
)

// isolateCLIConfigs points glab, gh, and tea configuration discovery at empty
// temp dirs so a real CLI install on the developer's machine cannot answer for
// one of the placeholder hosts below.
func isolateCLIConfigs(t *testing.T) {
	t.Helper()
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func fixedSSHHostname(hostname string) sshHostnameLookup {
	return func(context.Context, string) (string, error) { return hostname, nil }
}

func TestNormalizeForgejoSSHDomain(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "bare hostname", raw: "ssh.forge.example", want: "ssh.forge.example"},
		{name: "surrounding space", raw: "  ssh.forge.example  ", want: "ssh.forge.example"},
		{name: "mixed case", raw: "SSH.Forge.Example", want: "ssh.forge.example"},
		{name: "port dropped", raw: "ssh.forge.example:2222", want: "ssh.forge.example"},
		{name: "empty port dropped", raw: "ssh.forge.example:", want: "ssh.forge.example"},
		{name: "mixed case with a port", raw: "SSH.Forge.Example:2222", want: "ssh.forge.example"},
		{name: "bracketed IPv6 literal", raw: "[2001:db8::1]:2222", want: "2001:db8::1"},
		{name: "bracketed IPv6 loopback", raw: "[::1]", want: "::1"},
		{name: "IPv6 address in one spelling", raw: "[2001:DB8::0:1]", want: "2001:db8::1"},
		{name: "bracketed IPv6 loopback with a port", raw: "[::1]:2222", want: "::1"},
		{name: "IPv4 literal", raw: "127.0.0.1", want: "127.0.0.1"},
		{name: "IPv4 literal with a port", raw: "10.0.0.5:2222", want: "10.0.0.5"},
		{name: "hyphen inside a label", raw: "ssh-01.forge.example", want: "ssh-01.forge.example"},
		{name: "underscore inside a label", raw: "forgejo_ssh.internal", want: "forgejo_ssh.internal"},
		{name: "underscore inside a label with mixed case", raw: "Forgejo_SSH.Internal", want: "forgejo_ssh.internal"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeForgejoSSHDomain(tt.raw)
			if err != nil {
				t.Fatalf("NormalizeForgejoSSHDomain(%q) error = %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeForgejoSSHDomain(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestNormalizeForgejoSSHDomainRejectsNonHostnames(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "blank", raw: "   "},
		{name: "scheme", raw: "ssh://ssh.forge.example"},
		{name: "web scheme", raw: "https://ssh.forge.example"},
		{name: "path", raw: "ssh.forge.example/git"},
		{name: "credentials", raw: "git@ssh.forge.example"},
		{name: "query", raw: "ssh.forge.example?probe=1"},
		{name: "forced query", raw: "ssh.forge.example?"},
		{name: "fragment", raw: "ssh.forge.example#git"},
		{name: "backslash", raw: `ssh.forge.example\git`},
		{name: "non-numeric port", raw: "ssh.forge.example:port"},
		{name: "port only", raw: ":2222"},
		{name: "space inside", raw: "ssh forge example"},
		// An unbracketed IPv6 literal collapses to ":" once net/url reads the
		// trailing group as a port, so it could never match a resolved host.
		{name: "unbracketed IPv6 literal", raw: "::1"},
		{name: "dot dot", raw: ".."},
		{name: "single dot", raw: "."},
		{name: "wildcard", raw: "*.forge.example"},
		{name: "trailing dot", raw: "ssh.forge.example."},
		{name: "empty label", raw: "ssh..forge.example"},
		{name: "label starts with a hyphen", raw: "-ssh.forge.example"},
		{name: "label ends with a hyphen", raw: "ssh-.forge.example"},
		{name: "label starts with an underscore", raw: "_ssh.forge.example"},
		{name: "label ends with an underscore", raw: "ssh_.forge.example"},
		{name: "non-ASCII name", raw: "forgé.example"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeForgejoSSHDomain(tt.raw)
			if err == nil {
				t.Fatalf("NormalizeForgejoSSHDomain(%q) = %q, want an error", tt.raw, got)
			}
			if got != "" {
				t.Fatalf("NormalizeForgejoSSHDomain(%q) = %q, want an empty hostname", tt.raw, got)
			}
		})
	}
}

func TestNormalizeForgejoSSHDomainKeepsRejectedValuesOutOfTheError(t *testing.T) {
	t.Parallel()
	// A rejection reaches a step's skip reason, the step log, and the run
	// database, and an operator can paste a clone URL carrying a credential into
	// the setting, so no part of the value may appear in the error.
	const credential = "PLACEHOLDER-CREDENTIAL"
	for _, raw := range []string{
		"git:" + credential + "@ssh.forge.example",
		"ssh://git:" + credential + "@ssh.forge.example:2222",
		"ssh.forge.example?token=" + credential,
		"ssh.forge.example#" + credential,
	} {
		got, err := NormalizeForgejoSSHDomain(raw)
		if err == nil {
			t.Fatalf("NormalizeForgejoSSHDomain(%q) = %q, want an error", raw, got)
		}
		if strings.Contains(err.Error(), credential) {
			t.Fatalf("NormalizeForgejoSSHDomain() error = %v, want no part of the rejected value", err)
		}
		if strings.Contains(err.Error(), "ssh.forge.example") {
			t.Fatalf("NormalizeForgejoSSHDomain() error = %v, want no part of the rejected value", err)
		}
	}
}

func TestDetectProvider_DeclaredForgejoSSHDomainAcceptsSSHRemotes(t *testing.T) {
	isolateCLIConfigs(t)
	for _, tt := range []struct {
		name     string
		base     string
		remote   string
		domain   string
		resolved string
	}{
		{
			name:     "scp remote on the declared domain",
			base:     sshDomainBase,
			remote:   sshDomainRemote,
			domain:   sshDomainHost,
			resolved: sshDomainHost,
		},
		{
			name:     "ssh URL with a clone port",
			base:     sshDomainBase,
			remote:   "ssh://git@ssh.forge.example:2222/git/octo/widgets.git",
			domain:   sshDomainHost,
			resolved: sshDomainHost,
		},
		{
			name:     "scp alias resolved to the declared domain",
			base:     sshDomainBase,
			remote:   "git@forgejo-work:git/octo/widgets.git",
			domain:   sshDomainHost,
			resolved: sshDomainHost,
		},
		{
			name:     "ssh URL alias resolved to the declared domain",
			base:     sshDomainBase,
			remote:   "ssh://git@forgejo-work:2222/git/octo/widgets.git",
			domain:   sshDomainHost,
			resolved: sshDomainHost,
		},
		{
			name:     "declared domain carries a port and mixed case",
			base:     sshDomainBase,
			remote:   sshDomainRemote,
			domain:   "SSH.Forge.Example:2222",
			resolved: sshDomainHost,
		},
		{
			name:     "base URL without a path prefix",
			base:     "https://forge.example",
			remote:   "git@ssh.forge.example:octo/widgets.git",
			domain:   sshDomainHost,
			resolved: sshDomainHost,
		},
		{
			name:     "pull request URL path",
			base:     sshDomainBase,
			remote:   "ssh://git@ssh.forge.example:2222/git/octo/widgets/pulls/42",
			domain:   sshDomainHost,
			resolved: sshDomainHost,
		},
		{
			name:     "IPv4 clone host",
			base:     sshDomainBase,
			remote:   "git@10.0.0.5:git/octo/widgets.git",
			domain:   "10.0.0.5",
			resolved: "10.0.0.5",
		},
		{
			// Host extraction keeps the brackets an IPv6 remote carries, so the
			// declared address has to match through them.
			name:     "bracketed IPv6 clone host",
			base:     sshDomainBase,
			remote:   "ssh://git@[2001:db8::1]:2222/git/octo/widgets.git",
			domain:   "[2001:db8::1]",
			resolved: "[2001:db8::1]",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := detectProviderWithForgejo(
				context.Background(),
				tt.remote,
				ForgejoEnvironment{BaseURL: tt.base, SSHDomain: tt.domain},
				fixedSSHHostname(tt.resolved),
			)
			if got != ProviderForgejo {
				t.Fatalf("detectProviderWithForgejo(%q) = %q, want %q", tt.remote, got, ProviderForgejo)
			}
		})
	}
}

func TestDetectProvider_DeclaredForgejoSSHDomainFailsClosed(t *testing.T) {
	isolateCLIConfigs(t)
	for _, tt := range []struct {
		name   string
		base   string
		remote string
		domain string
		// resolved overrides what the SSH lookup answers. An empty value puts
		// the remote on the declared domain, so the case is rejected for the
		// reason its name gives rather than for a host difference.
		resolved string
	}{
		{name: "mismatched path prefix", base: sshDomainBase, remote: "git@ssh.forge.example:other/octo/widgets.git", domain: sshDomainHost},
		{name: "path without OWNER/REPO", base: sshDomainBase, remote: "git@ssh.forge.example:git/widgets.git", domain: sshDomainHost},
		{name: "no base URL", base: "", remote: sshDomainRemote, domain: sshDomainHost},
		{name: "blank base URL", base: "   ", remote: sshDomainRemote, domain: sshDomainHost},
		{name: "invalid base URL scheme", base: "ftp://forge.example/git", remote: sshDomainRemote, domain: sshDomainHost},
		{name: "another host", base: sshDomainBase, remote: "git@other.example:git/octo/widgets.git", domain: sshDomainHost, resolved: "other.example"},
		{name: "HTTPS remote on the declared domain", base: sshDomainBase, remote: "https://ssh.forge.example:3443/git/octo/widgets.git", domain: sshDomainHost},
		{name: "empty domain", base: sshDomainBase, remote: sshDomainRemote, domain: ""},
		{name: "blank domain", base: sshDomainBase, remote: sshDomainRemote, domain: "   "},
		{name: "domain with a scheme", base: sshDomainBase, remote: sshDomainRemote, domain: "ssh://ssh.forge.example"},
		{name: "domain with a path", base: sshDomainBase, remote: sshDomainRemote, domain: "ssh.forge.example/git"},
		{name: "domain with credentials", base: sshDomainBase, remote: sshDomainRemote, domain: "git@ssh.forge.example"},
		{name: "domain with a query", base: sshDomainBase, remote: sshDomainRemote, domain: "ssh.forge.example?probe=1"},
		{name: "domain with a fragment", base: sshDomainBase, remote: sshDomainRemote, domain: "ssh.forge.example#git"},
		{name: "domain with a non-numeric port", base: sshDomainBase, remote: sshDomainRemote, domain: "ssh.forge.example:port"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resolved := tt.resolved
			if resolved == "" {
				resolved = sshDomainHost
			}
			got := detectProviderWithForgejo(
				context.Background(),
				tt.remote,
				ForgejoEnvironment{BaseURL: tt.base, SSHDomain: tt.domain},
				fixedSSHHostname(resolved),
			)
			if got != ProviderUnknown {
				t.Fatalf("detectProviderWithForgejo(%q) = %q, want %q", tt.remote, got, ProviderUnknown)
			}
		})
	}
}

func TestDetectProvider_DeclaredForgejoSSHDomainDoesNotOverrideHostedProviders(t *testing.T) {
	isolateCLIConfigs(t)
	for _, tt := range []struct {
		remote string
		want   Provider
	}{
		{"git@github.com:octo/widgets.git", ProviderGitHub},
		{"git@gitlab.com:octo/widgets.git", ProviderGitLab},
		{"git@bitbucket.org:octo/widgets.git", ProviderBitbucket},
		{"git@ssh.dev.azure.com:octo/widgets.git", ProviderAzureDevOps},
	} {
		host := ExtractHost(tt.remote)
		// The declared domain is the remote's own host and the path matches the
		// base prefix, so only provider precedence keeps this from Forgejo.
		got := detectProviderWithForgejo(
			context.Background(),
			tt.remote,
			ForgejoEnvironment{BaseURL: "https://forge.example", SSHDomain: host},
			fixedSSHHostname(host),
		)
		if got != tt.want {
			t.Errorf("detectProviderWithForgejo(%q) = %q, want %q", tt.remote, got, tt.want)
		}
	}
}

func TestDetectProvider_DeclaredForgejoSSHDomainDoesNotOverrideConfiguredCLIHosts(t *testing.T) {
	remote := "git@code.example:octo/widgets.git"
	forgejoEnv := ForgejoEnvironment{BaseURL: "https://forge.example", SSHDomain: "code.example"}

	t.Run("glab host", func(t *testing.T) {
		isolateCLIConfigs(t)
		writeGlabConfig(t, `hosts:
    code.example:
        token: xxx
        api_host: code.example
        api_protocol: https
`)
		got := detectProviderWithForgejo(context.Background(), remote, forgejoEnv, fixedSSHHostname("code.example"))
		if got != ProviderGitLab {
			t.Fatalf("detectProviderWithForgejo() = %q, want %q", got, ProviderGitLab)
		}
	})

	t.Run("gh host", func(t *testing.T) {
		isolateCLIConfigs(t)
		writeGhConfig(t, `code.example:
    user: someuser
    oauth_token: xxx
    git_protocol: ssh
`)
		got := detectProviderWithForgejo(context.Background(), remote, forgejoEnv, fixedSSHHostname("code.example"))
		if got != ProviderGitHub {
			t.Fatalf("detectProviderWithForgejo() = %q, want %q", got, ProviderGitHub)
		}
	})

	t.Run("tea host", func(t *testing.T) {
		isolateCLIConfigs(t)
		writeTeaConfig(t, `logins:
    - name: work
      url: https://code.example
      ssh_host: code.example
      user: someuser
      token: xxx
`)
		got := detectProviderWithForgejo(context.Background(), remote, forgejoEnv, fixedSSHHostname("code.example"))
		if got != ProviderGitea {
			t.Fatalf("detectProviderWithForgejo() = %q, want %q", got, ProviderGitea)
		}
	})
}

func TestDetectProviderContextWithForgejo_AcceptsDeclaredSSHDomain(t *testing.T) {
	isolateCLIConfigs(t)
	forgejoEnv := ForgejoEnvironment{BaseURL: sshDomainBase, SSHDomain: sshDomainHost}
	if got := DetectProviderContextWithForgejo(context.Background(), sshDomainRemote, forgejoEnv); got != ProviderForgejo {
		t.Fatalf("DetectProviderContextWithForgejo() = %q, want %q", got, ProviderForgejo)
	}
	// The base URL alone cannot see an origin published on another hostname.
	if got := DetectProviderContextWithForgejoBaseURL(context.Background(), sshDomainRemote, sshDomainBase); got != ProviderUnknown {
		t.Fatalf("DetectProviderContextWithForgejoBaseURL() = %q, want %q", got, ProviderUnknown)
	}
}

func TestDetectProvider_ReadsForgejoSSHDomainFromEnvironment(t *testing.T) {
	isolateCLIConfigs(t)
	t.Setenv("FORGEJO_BASE_URL", sshDomainBase)
	t.Setenv("FORGEJO_SSH_DOMAIN", sshDomainHost)
	if got := DetectProvider(sshDomainRemote); got != ProviderForgejo {
		t.Fatalf("DetectProvider() = %q, want %q", got, ProviderForgejo)
	}

	// Without the base URL the SSH domain alone is inert.
	t.Setenv("FORGEJO_BASE_URL", "")
	if got := DetectProvider(sshDomainRemote); got != ProviderUnknown {
		t.Fatalf("DetectProvider() without a base URL = %q, want %q", got, ProviderUnknown)
	}
}
