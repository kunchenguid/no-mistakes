package scm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Provider string

const (
	ProviderGitHub    Provider = "github"
	ProviderGitLab    Provider = "gitlab"
	ProviderBitbucket Provider = "bitbucket"
	ProviderUnknown   Provider = "unknown"
)

func DetectProvider(url string) Provider {
	lower := strings.ToLower(url)
	switch {
	case strings.Contains(lower, "github.com"):
		return ProviderGitHub
	case strings.Contains(lower, "gitlab.com") || strings.Contains(lower, "gitlab."):
		return ProviderGitLab
	case strings.Contains(lower, "bitbucket.org"):
		return ProviderBitbucket
	}

	// Fallback for self-hosted GitLab instances whose hostname carries no
	// "gitlab" marker: consult the glab CLI's configured hosts. If the remote's
	// host (or a host's api_host) is one glab is configured to talk to, treat it
	// as GitLab. This reads whatever the user configured at runtime; no host is
	// hardcoded.
	if host := ExtractHost(url); host != "" {
		if glabKnowsHost(host) {
			return ProviderGitLab
		}
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

func (p Provider) CLIName() string {
	switch p {
	case ProviderGitHub:
		return "gh"
	case ProviderGitLab:
		return "glab"
	case ProviderBitbucket:
		return "bb"
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
	return cmd.Run() == nil
}
