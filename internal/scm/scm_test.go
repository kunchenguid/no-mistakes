package scm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectProvider(t *testing.T) {
	// Point glab and gh configs at empty temp dirs so a real CLI install on the
	// host cannot influence the substring-based assertions below.
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_CONFIG_DIR", t.TempDir())

	tests := []struct {
		url  string
		want Provider
	}{
		{"https://github.com/user/repo.git", ProviderGitHub},
		{"git@github.com:user/repo.git", ProviderGitHub},
		{"https://gitlab.com/user/repo.git", ProviderGitLab},
		{"https://gitlab.mycorp.com/group/repo.git", ProviderGitLab},
		{"https://bitbucket.org/user/repo.git", ProviderBitbucket},
		{"https://dev.azure.com/org/project/_git/repo", ProviderAzureDevOps},
		{"git@ssh.dev.azure.com:v3/org/project/repo", ProviderAzureDevOps},
		{"https://org.visualstudio.com/project/_git/repo", ProviderAzureDevOps},
		{"https://codeberg.org/user/repo.git", ProviderForgejo},
		{"https://codeberg.org/user/github.com.git", ProviderForgejo},
		{"https://forgejo.example/user/repo.git", ProviderForgejo},
		{"https://forgejo.gitlab.example/user/repo.git", ProviderForgejo},
		{"https://forgejo.github.com.example/user/repo.git", ProviderForgejo},
		{"https://example.com/user/repo.git", ProviderUnknown},
	}

	for _, tt := range tests {
		if got := DetectProvider(tt.url); got != tt.want {
			t.Errorf("DetectProvider(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestDetectProvider_SSHHostAlias(t *testing.T) {
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("FORGEJO_BASE_URL", "https://github.com")

	tests := []struct {
		name     string
		url      string
		hostname string
		want     Provider
	}{
		{
			name:     "GitHub scp remote",
			url:      "git@github-personal:owner/repo.git",
			hostname: "github.com",
			want:     ProviderGitHub,
		},
		{
			name:     "GitLab SSH URL",
			url:      "ssh://git@gitlab-work/group/repo.git",
			hostname: "gitlab.com",
			want:     ProviderGitLab,
		},
		{
			name:     "Forgejo-named GitHub alias",
			url:      "git@forgejo-github:owner/repo.git",
			hostname: "github.com",
			want:     ProviderGitHub,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectProvider(context.Background(), tt.url, func(context.Context, string) (string, error) {
				return tt.hostname, nil
			})
			if got != tt.want {
				t.Fatalf("detectProvider(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestResolveHost_SSHConfigLookup(t *testing.T) {
	t.Run("canonical hostname", func(t *testing.T) {
		got := resolveHost(context.Background(), "git@github-personal:owner/repo.git", func(_ context.Context, alias string) (string, error) {
			if alias != "github-personal" {
				t.Fatalf("alias = %q, want github-personal", alias)
			}
			return "GitHub.COM", nil
		})
		if got != "github.com" {
			t.Fatalf("resolveHost() = %q, want github.com", got)
		}
	})

	t.Run("lookup failure preserves alias", func(t *testing.T) {
		got := resolveHost(context.Background(), "git@github-personal:owner/repo.git", func(context.Context, string) (string, error) {
			return "", errors.New("ssh unavailable")
		})
		if got != "github-personal" {
			t.Fatalf("resolveHost() = %q, want github-personal", got)
		}
	})

	t.Run("HTTPS does not invoke SSH", func(t *testing.T) {
		got := resolveHost(context.Background(), "https://code.example.com/owner/repo.git", func(context.Context, string) (string, error) {
			t.Fatal("SSH lookup invoked for HTTPS remote")
			return "", nil
		})
		if got != "code.example.com" {
			t.Fatalf("resolveHost() = %q, want code.example.com", got)
		}
	})

	t.Run("Windows path does not invoke SSH", func(t *testing.T) {
		got := resolveHost(context.Background(), `C:\repo`, func(context.Context, string) (string, error) {
			t.Fatal("SSH lookup invoked for Windows path")
			return "", nil
		})
		if got != "c" {
			t.Fatalf("resolveHost() = %q, want c", got)
		}
	})
}

func TestDetectProvider_ConfiguredForgejoBaseResolvesSSHHostAlias(t *testing.T) {
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_CONFIG_DIR", t.TempDir())

	got := detectProviderWithForgejoBaseURL(
		context.Background(),
		"git@github.com-work:scm/octo/widgets.git",
		"https://forgejo.gitlab.example:3443/scm",
		func(_ context.Context, alias string) (string, error) {
			if alias != "github.com-work" {
				t.Fatalf("alias = %q, want github.com-work", alias)
			}
			return "forgejo.gitlab.example", nil
		},
	)
	if got != ProviderForgejo {
		t.Fatalf("detectProviderWithForgejoBaseURL() = %q, want %q", got, ProviderForgejo)
	}
}

func TestDetectProvider_ConfiguredForgejoBaseSupportsSelfHostedPrefixesAndPorts(t *testing.T) {
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_CONFIG_DIR", t.TempDir())

	base := "https://code.example:3443/scm"
	for _, remote := range []string{
		"https://code.example:3443/scm/octo/widgets.git",
		"https://code.example:3443/scm/octo/widgets/pulls/42",
		"ssh://git@code.example:2222/scm/octo/widgets.git",
		"git@code.example:scm/octo/widgets.git",
	} {
		if got := DetectProviderWithForgejoBaseURL(remote, base); got != ProviderForgejo {
			t.Errorf("DetectProviderWithForgejoBaseURL(%q) = %q, want %q", remote, got, ProviderForgejo)
		}
	}
	if got := DetectProviderWithForgejoBaseURL("https://code.example:3443/other/octo/widgets.git", base); got != ProviderUnknown {
		t.Errorf("mismatched path prefix detected as %q, want unknown", got)
	}
	base = "https://forgejo.gitlab.example:3443/scm"
	if got := DetectProviderWithForgejoBaseURL(base+"/octo/widgets.git", base); got != ProviderForgejo {
		t.Errorf("configured Forgejo host with provider substring detected as %q, want %q", got, ProviderForgejo)
	}
}

func TestDetectProvider_ConfiguredForgejoBaseUsesAdapterURLRules(t *testing.T) {
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_CONFIG_DIR", t.TempDir())

	for _, tt := range []struct {
		name   string
		base   string
		remote string
		want   Provider
	}{
		{
			name:   "canonical path",
			base:   "https://code.example/scm/../forgejo",
			remote: "https://code.example/forgejo/octo/widgets.git",
			want:   ProviderForgejo,
		},
		{
			name:   "unsupported base scheme",
			base:   "ftp://code.example/forgejo",
			remote: "ftp://code.example/forgejo/octo/widgets.git",
			want:   ProviderUnknown,
		},
		{
			name:   "unsupported remote scheme",
			base:   "https://code.example/forgejo",
			remote: "ftp://code.example/forgejo/octo/widgets.git",
			want:   ProviderUnknown,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectProviderWithForgejoBaseURL(tt.remote, tt.base); got != tt.want {
				t.Fatalf("DetectProviderWithForgejoBaseURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectProvider_ForgejoConfigDoesNotOverrideKnownProviders(t *testing.T) {
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	for _, tt := range []struct {
		remote string
		want   Provider
	}{
		{"https://github.com/octo/widgets.git", ProviderGitHub},
		{"https://gitlab.com/octo/widgets.git", ProviderGitLab},
		{"https://bitbucket.org/octo/widgets.git", ProviderBitbucket},
		{"https://dev.azure.com/octo/project/_git/widgets", ProviderAzureDevOps},
	} {
		if got := DetectProviderWithForgejoBaseURL(tt.remote, "https://"+ExtractHost(tt.remote)); got != tt.want {
			t.Errorf("known provider %q detected as %q, want %q", tt.remote, got, tt.want)
		}
	}
}

func TestDetectProvider_UsesForgejoBaseEnvironment(t *testing.T) {
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	tests := []struct {
		name   string
		base   string
		remote string
	}{
		{name: "non-default port and prefix", base: "https://code.example:3443/scm", remote: "https://code.example:3443/scm/octo/widgets.git"},
		{name: "configured HTTPS default port", base: "https://code.example:443", remote: "https://code.example/octo/widgets.git"},
		{name: "remote HTTPS default port", base: "https://code.example", remote: "https://code.example:443/octo/widgets.git"},
		{name: "HTTP default port", base: "http://CODE.EXAMPLE:80", remote: "http://code.example/octo/widgets.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("FORGEJO_BASE_URL", tt.base)
			if got := DetectProvider(tt.remote); got != ProviderForgejo {
				t.Fatalf("DetectProvider() = %q, want %q", got, ProviderForgejo)
			}
		})
	}
}

// writeGlabConfig writes a synthetic glab config.yml into a temp dir and points
// GLAB_CONFIG_DIR at it. The host names are placeholders only.
func writeGlabConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GLAB_CONFIG_DIR", dir)
}

func TestDetectProvider_SelfHostedGitLabViaGlabConfig(t *testing.T) {
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("FORGEJO_BASE_URL", "https://gitlab.example.com")
	writeGlabConfig(t, `hosts:
    gitlab.example.com:
        token: xxx
        api_host: gitlab.example.com
        api_protocol: https
`)

	cases := []string{
		"https://gitlab.example.com/group/repo.git",
		"git@gitlab.example.com:group/repo.git",
		"ssh://git@gitlab.example.com:22/group/repo.git",
	}
	for _, url := range cases {
		if got := DetectProvider(url); got != ProviderGitLab {
			t.Errorf("DetectProvider(%q) = %q, want %q", url, got, ProviderGitLab)
		}
	}

	// A host not in the config still resolves to unknown.
	if got := DetectProvider("https://other.example.org/group/repo.git"); got != ProviderUnknown {
		t.Errorf("DetectProvider(unconfigured host) = %q, want %q", got, ProviderUnknown)
	}
}

func TestDetectProvider_SelfHostedGitLabViaAPIHost(t *testing.T) {
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	// The remote host differs from the config key but matches api_host.
	writeGlabConfig(t, `hosts:
    git.example.com:
        token: xxx
        api_host: api.example.com
`)

	if got := DetectProvider("https://api.example.com/group/repo.git"); got != ProviderGitLab {
		t.Errorf("DetectProvider(api_host match) = %q, want %q", got, ProviderGitLab)
	}
	if got := DetectProvider("https://git.example.com/group/repo.git"); got != ProviderGitLab {
		t.Errorf("DetectProvider(host key match) = %q, want %q", got, ProviderGitLab)
	}
}

func TestDetectProvider_GlabConfigMissingFailsClosed(t *testing.T) {
	// Both CLI configs point at empty dirs: no config files present.
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	if got := DetectProvider("https://selfhosted.example.com/group/repo.git"); got != ProviderUnknown {
		t.Errorf("DetectProvider(no glab config) = %q, want %q", got, ProviderUnknown)
	}
}

func TestDetectProvider_GlabConfigMalformedFailsClosed(t *testing.T) {
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	writeGlabConfig(t, "this: is: not: valid: yaml: ::::\n\t- broken")
	if got := DetectProvider("https://selfhosted.example.com/group/repo.git"); got != ProviderUnknown {
		t.Errorf("DetectProvider(malformed glab config) = %q, want %q", got, ProviderUnknown)
	}
}

// writeGhConfig writes a synthetic gh hosts.yml into a temp dir and points
// GH_CONFIG_DIR at it. The host names are placeholders only.
func writeGhConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CONFIG_DIR", dir)
}

func TestDetectProvider_GHEViaGhConfig(t *testing.T) {
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("FORGEJO_BASE_URL", "https://bbgithub.dev.bloomberg.com")
	writeGhConfig(t, `bbgithub.dev.bloomberg.com:
    user: someuser
    oauth_token: xxx
    git_protocol: ssh
`)

	cases := []string{
		"git@bbgithub.dev.bloomberg.com:org/repo.git",
		"https://bbgithub.dev.bloomberg.com/org/repo.git",
		"ssh://git@bbgithub.dev.bloomberg.com/org/repo.git",
	}
	for _, url := range cases {
		if got := DetectProvider(url); got != ProviderGitHub {
			t.Errorf("DetectProvider(%q) = %q, want %q", url, got, ProviderGitHub)
		}
	}

	// A host not in the config still resolves to unknown.
	if got := DetectProvider("https://other.example.org/org/repo.git"); got != ProviderUnknown {
		t.Errorf("DetectProvider(unconfigured GHE host) = %q, want %q", got, ProviderUnknown)
	}
}

func TestDetectProvider_GhConfigMissingFailsClosed(t *testing.T) {
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	if got := DetectProvider("https://ghe.example.com/org/repo.git"); got != ProviderUnknown {
		t.Errorf("DetectProvider(no gh config) = %q, want %q", got, ProviderUnknown)
	}
}

func TestDetectProvider_GhConfigMalformedFailsClosed(t *testing.T) {
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	writeGhConfig(t, "this: is: not: valid: yaml: ::::\n\t- broken")
	if got := DetectProvider("https://ghe.example.com/org/repo.git"); got != ProviderUnknown {
		t.Errorf("DetectProvider(malformed gh config) = %q, want %q", got, ProviderUnknown)
	}
}
