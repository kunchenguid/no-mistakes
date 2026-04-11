package scm

import "testing"

func TestDetectProvider(t *testing.T) {
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

func TestProviderCLIName(t *testing.T) {
	tests := []struct {
		provider Provider
		want     string
	}{
		{ProviderGitHub, "gh"},
		{ProviderGitLab, "glab"},
		{ProviderBitbucket, "bb"},
		{ProviderUnknown, ""},
	}

	for _, tt := range tests {
		if got := tt.provider.CLIName(); got != tt.want {
			t.Errorf("%q.CLIName() = %q, want %q", tt.provider, got, tt.want)
		}
	}
}
