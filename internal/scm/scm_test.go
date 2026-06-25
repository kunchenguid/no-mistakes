package scm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectProvider(t *testing.T) {
	// Point glab config at an empty temp dir so a real glab install on the host
	// cannot influence the substring-based assertions below.
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())

	tests := []struct {
		url  string
		want Provider
	}{
		{"https://github.com/user/repo.git", ProviderGitHub},
		{"git@github.com:user/repo.git", ProviderGitHub},
		{"https://gitlab.com/user/repo.git", ProviderGitLab},
		{"https://gitlab.mycorp.com/group/repo.git", ProviderGitLab},
		{"https://bitbucket.org/user/repo.git", ProviderBitbucket},
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
	// GLAB_CONFIG_DIR points at an empty dir: no config file present.
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	if got := DetectProvider("https://selfhosted.example.com/group/repo.git"); got != ProviderUnknown {
		t.Errorf("DetectProvider(no glab config) = %q, want %q", got, ProviderUnknown)
	}
}

func TestDetectProvider_GlabConfigMalformedFailsClosed(t *testing.T) {
	writeGlabConfig(t, "this: is: not: valid: yaml: ::::\n\t- broken")
	if got := DetectProvider("https://selfhosted.example.com/group/repo.git"); got != ProviderUnknown {
		t.Errorf("DetectProvider(malformed glab config) = %q, want %q", got, ProviderUnknown)
	}
}
