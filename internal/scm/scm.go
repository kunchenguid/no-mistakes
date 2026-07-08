package scm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"gopkg.in/yaml.v3"
)

type Provider string

const (
	ProviderGitHub      Provider = "github"
	ProviderGitLab      Provider = "gitlab"
	ProviderBitbucket   Provider = "bitbucket"
	ProviderAzureDevOps Provider = "azuredevops"
	ProviderUnknown     Provider = "unknown"
)

func DetectProvider(url string) Provider {
	if p := providerFromMarker(url); p != ProviderUnknown {
		return p
	}

	// Fallback for self-hosted GitLab instances whose hostname carries no
	// "gitlab" marker: consult the glab CLI's configured hosts. If the remote's
	// host (or a host's api_host) is one glab is configured to talk to, treat it
	// as GitLab. This reads whatever the user configured at runtime; no host is
	// hardcoded.
	//
	// Fallback for GitHub Enterprise Server instances: consult the gh CLI's
	// configured hosts (hosts.yml). If the remote's host is one gh is
	// authenticated with, treat it as GitHub.
	host := ExtractHost(url)
	if host == "" {
		return ProviderUnknown
	}
	if p := providerFromKnownHost(host); p != ProviderUnknown {
		return p
	}

	// Fallback for SSH host aliases (a `Host github-work` block in ~/.ssh/config
	// that maps to a real HostName). The push step works because git resolves
	// the alias itself, but the literal host ("github-work") matches no marker,
	// so pr/ci would silently skip (issue #290). Resolve the alias to its real
	// host and re-classify. Only attempted for ssh/scp remotes, and fail-closed:
	// an unresolved alias leaves detection unchanged.
	if remoteUsesSSH(url) {
		if resolved := ResolveHostAlias(host); !strings.EqualFold(resolved, host) {
			if p := providerFromMarker(resolved); p != ProviderUnknown {
				return p
			}
			if p := providerFromKnownHost(resolved); p != ProviderUnknown {
				return p
			}
		}
	}

	return ProviderUnknown
}

// providerFromMarker classifies a remote URL or host by well-known hostname
// markers. It returns ProviderUnknown when no marker matches.
func providerFromMarker(s string) Provider {
	lower := strings.ToLower(s)
	switch {
	case strings.Contains(lower, "github.com"):
		return ProviderGitHub
	case strings.Contains(lower, "gitlab.com") || strings.Contains(lower, "gitlab."):
		return ProviderGitLab
	case strings.Contains(lower, "bitbucket.org"):
		return ProviderBitbucket
	case strings.Contains(lower, "dev.azure.com") || strings.Contains(lower, "visualstudio.com"):
		// Covers dev.azure.com, ssh.dev.azure.com, {org}.visualstudio.com, and
		// the legacy vs-ssh.visualstudio.com SSH host.
		return ProviderAzureDevOps
	}
	return ProviderUnknown
}

// providerFromKnownHost classifies a bare host by consulting the provider CLIs'
// configured hosts (glab, then gh), for self-hosted GitLab / GitHub Enterprise
// instances whose hostname carries no marker. It returns ProviderUnknown when
// neither CLI recognizes the host.
func providerFromKnownHost(host string) Provider {
	if glabKnowsHost(host) {
		return ProviderGitLab
	}
	if ghKnowsHost(host) {
		return ProviderGitHub
	}
	return ProviderUnknown
}

// glabKnowsHost reports whether host appears in glab's configured hosts map,
// either as a top-level key or as a host's api_host. Any read/parse error is
// treated as "not configured" so detection fails closed to ProviderUnknown.
func glabKnowsHost(host string) bool {
	path := glabConfigPath()
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cfg struct {
		Hosts map[string]struct {
			APIHost string `yaml:"api_host"`
		} `yaml:"hosts"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return false
	}
	host = strings.ToLower(host)
	for key, h := range cfg.Hosts {
		if strings.ToLower(strings.TrimSpace(key)) == host {
			return true
		}
		if api := strings.ToLower(strings.TrimSpace(h.APIHost)); api != "" && ExtractHost(api) == host {
			return true
		}
	}
	return false
}

// glabConfigPath resolves glab's config file location, preferring
// $GLAB_CONFIG_DIR, then $XDG_CONFIG_HOME/glab-cli, then ~/.config/glab-cli.
// It returns "" when no home/config directory can be determined.
func glabConfigPath() string {
	if dir := os.Getenv("GLAB_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "config.yml")
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "glab-cli", "config.yml")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "glab-cli", "config.yml")
}

// ghKnowsHost reports whether host appears as a top-level key in gh's
// hosts.yml. Any read/parse error is treated as "not configured" so detection
// fails closed to ProviderUnknown.
func ghKnowsHost(host string) bool {
	path := ghConfigPath()
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var hosts map[string]interface{}
	if err := yaml.Unmarshal(data, &hosts); err != nil {
		return false
	}
	host = strings.ToLower(host)
	for key := range hosts {
		if strings.ToLower(strings.TrimSpace(key)) == host {
			return true
		}
	}
	return false
}

// ghConfigPath resolves gh's hosts config file location, preferring
// $GH_CONFIG_DIR, then $XDG_CONFIG_HOME/gh, then ~/.config/gh.
// It returns "" when no home/config directory can be determined.
func ghConfigPath() string {
	if dir := os.Getenv("GH_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "hosts.yml")
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "gh", "hosts.yml")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "gh", "hosts.yml")
}

func (p Provider) CLIName() string {
	switch p {
	case ProviderGitHub:
		return "gh"
	case ProviderGitLab:
		return "glab"
	case ProviderBitbucket:
		return "bb"
	case ProviderAzureDevOps:
		return "az"
	default:
		return ""
	}
}

func (p Provider) AuthCheckCommand() []string {
	switch p {
	case ProviderGitHub:
		return []string{"gh", "auth", "status"}
	case ProviderGitLab:
		return []string{"glab", "auth", "status"}
	case ProviderBitbucket:
		return []string{"bb", "profile", "which"}
	case ProviderAzureDevOps:
		return []string{"az", "account", "show"}
	default:
		return nil
	}
}

func CLIAvailable(provider Provider) bool {
	name := provider.CLIName()
	if name == "" {
		return false
	}
	_, err := exec.LookPath(name)
	return err == nil
}

func AuthConfigured(ctx context.Context, provider Provider, workDir string) bool {
	args := provider.AuthCheckCommand()
	if len(args) == 0 {
		return false
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = workDir
	winproc.Harden(cmd)
	return cmd.Run() == nil
}
