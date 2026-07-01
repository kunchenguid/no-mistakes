package scm

import (
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
		{"https://example.com/user/repo.git", ProviderUnknown},
	}

	for _, tt := range tests {
		if got := DetectProvider(tt.url); got != tt.want {
			t.Errorf("DetectProvider(%q) = %q, want %q", tt.url, got, tt.want)
		}
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
