package scm

import (
	"context"
	"os/exec"
	"strings"
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
	default:
		return ProviderUnknown
	}
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
