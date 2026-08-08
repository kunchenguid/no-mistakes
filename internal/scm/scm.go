package scm

import (
	"context"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"gopkg.in/yaml.v3"
)

type Provider string

const (
	ProviderGitHub      Provider = "github"
	ProviderGitLab      Provider = "gitlab"
	ProviderBitbucket   Provider = "bitbucket"
	ProviderAzureDevOps Provider = "azuredevops"
	ProviderForgejo     Provider = "forgejo"
	ProviderUnknown     Provider = "unknown"
)

type sshHostnameLookup func(context.Context, string) (string, error)

// DetectProvider identifies the SCM provider for url. SSH host aliases are
// resolved through the user's SSH configuration before detection falls back to
// ProviderUnknown.
func DetectProvider(remoteURL string) Provider {
	return DetectProviderContext(context.Background(), remoteURL)
}

// DetectProviderContext is DetectProvider with caller-controlled cancellation.
func DetectProviderContext(ctx context.Context, remoteURL string) Provider {
	return detectProvider(ctx, remoteURL, lookupSSHHostname)
}

// DetectProviderWithForgejoBaseURL detects a provider while allowing an
// explicit forgejo-axi base URL to identify an otherwise-unrecognizable
// self-hosted Forgejo origin. Known hosted providers keep precedence so a
// stray Forgejo setting cannot reroute GitHub, GitLab, Bitbucket, or Azure.
func DetectProviderWithForgejoBaseURL(remoteURL, forgejoBaseURL string) Provider {
	return DetectProviderContextWithForgejoBaseURL(context.Background(), remoteURL, forgejoBaseURL)
}

// DetectProviderContextWithForgejoBaseURL is DetectProviderWithForgejoBaseURL
// with caller-controlled cancellation for SSH host alias resolution.
func DetectProviderContextWithForgejoBaseURL(ctx context.Context, remoteURL, forgejoBaseURL string) Provider {
	return detectProviderWithForgejoBaseURL(ctx, remoteURL, forgejoBaseURL, lookupSSHHostname)
}

func detectProvider(ctx context.Context, remoteURL string, lookup sshHostnameLookup) Provider {
	return detectProviderWithForgejoBaseURL(ctx, remoteURL, os.Getenv("FORGEJO_BASE_URL"), lookup)
}

func detectProviderWithForgejoBaseURL(ctx context.Context, remoteURL, forgejoBaseURL string, lookup sshHostnameLookup) Provider {
	originalHost := ExtractHost(remoteURL)
	host := resolveHost(ctx, remoteURL, lookup)
	if host == "" {
		return ProviderUnknown
	}
	if provider := detectHostedProvider(host); provider != ProviderUnknown {
		return provider
	}
	if glabKnowsHost(host) {
		return ProviderGitLab
	}
	if ghKnowsHost(host) {
		return ProviderGitHub
	}
	if strings.EqualFold(host, originalHost) {
		if forgejoBaseMatchesRemote(forgejoBaseURL, remoteURL) {
			return ProviderForgejo
		}
	} else if forgejoBaseMatchesResolvedRemote(forgejoBaseURL, remoteURL, host) {
		return ProviderForgejo
	}
	if host == "codeberg.org" || strings.Contains(host, "forgejo") {
		return ProviderForgejo
	}
	return detectLegacyProviderHost(host)
}

func detectHostedProvider(host string) Provider {
	switch {
	case host == "github.com" || strings.HasSuffix(host, ".github.com"):
		return ProviderGitHub
	case host == "gitlab.com" || strings.HasSuffix(host, ".gitlab.com"):
		return ProviderGitLab
	case host == "bitbucket.org" || strings.HasSuffix(host, ".bitbucket.org"):
		return ProviderBitbucket
	case host == "dev.azure.com" || strings.HasSuffix(host, ".dev.azure.com") || strings.HasSuffix(host, ".visualstudio.com"):
		return ProviderAzureDevOps
	default:
		return ProviderUnknown
	}
}

func detectLegacyProviderHost(host string) Provider {
	switch {
	case strings.Contains(host, "github.com"):
		return ProviderGitHub
	case strings.Contains(host, "gitlab.com") || strings.Contains(host, "gitlab."):
		return ProviderGitLab
	case strings.Contains(host, "bitbucket.org"):
		return ProviderBitbucket
	case strings.Contains(host, "dev.azure.com") || strings.Contains(host, "visualstudio.com"):
		return ProviderAzureDevOps
	default:
		return ProviderUnknown
	}
}

// ResolveHost returns the canonical host for a remote. For SSH remotes it
// honors HostName mappings from the user's SSH configuration while preserving
// the original remote URL for all Git operations.
func ResolveHost(ctx context.Context, remote string) string {
	return resolveHost(ctx, remote, lookupSSHHostname)
}

func resolveHost(ctx context.Context, remote string, lookup sshHostnameLookup) string {
	host := ExtractHost(remote)
	if host == "" || !isSSHRemote(remote) || lookup == nil {
		return host
	}

	resolved, err := lookup(ctx, host)
	if err != nil {
		return host
	}
	resolved = strings.ToLower(strings.TrimSpace(stripPort(resolved)))
	if resolved == "" {
		return host
	}
	return resolved
}

func isSSHRemote(remote string) bool {
	remote = strings.TrimSpace(remote)
	lower := strings.ToLower(remote)
	if strings.HasPrefix(lower, "ssh://") {
		return true
	}
	if strings.Contains(remote, "://") {
		return false
	}
	colon := strings.IndexByte(remote, ':')
	if colon <= 0 {
		return false
	}
	if colon == 1 && len(remote) >= 3 && ((remote[0] >= 'a' && remote[0] <= 'z') || (remote[0] >= 'A' && remote[0] <= 'Z')) && (remote[2] == '/' || remote[2] == '\\') {
		return false
	}
	slash := strings.IndexAny(remote, `/\\`)
	return slash < 0 || colon < slash
}

func lookupSSHHostname(ctx context.Context, alias string) (string, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(lookupCtx, "ssh", "-G", "--", alias)
	shellenv.ConfigureShellCommand(cmd)
	out, err := shellenv.OutputShellCommand(cmd)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], "hostname") {
			return fields[1], nil
		}
	}
	return "", nil
}

func forgejoBaseMatchesRemote(baseRaw, remoteRaw string) bool {
	base, err := url.Parse(strings.TrimSpace(baseRaw))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return false
	}
	remoteHost, remotePath, remoteScheme, ok := providerRemoteParts(remoteRaw)
	if !ok {
		return false
	}
	if remoteScheme == "http" || remoteScheme == "https" {
		if normalizedHTTPAuthority(remoteScheme, remoteHost) != normalizedHTTPAuthority(base.Scheme, base.Host) {
			return false
		}
	} else if !strings.EqualFold(stripPort(remoteHost), base.Hostname()) {
		// SSH clone ports commonly differ from the Forgejo web port.
		return false
	}
	return forgejoPathsMatch(base.Path, remotePath)
}

func normalizedHTTPAuthority(scheme, authority string) string {
	parsed := &url.URL{Host: authority}
	hostname := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (strings.EqualFold(scheme, "http") && port == "80") || (strings.EqualFold(scheme, "https") && port == "443") {
		port = ""
	}
	if port != "" {
		return net.JoinHostPort(hostname, port)
	}
	if strings.Contains(hostname, ":") {
		return "[" + hostname + "]"
	}
	return hostname
}

func forgejoBaseMatchesResolvedRemote(baseRaw, remoteRaw, resolvedHost string) bool {
	base, err := url.Parse(strings.TrimSpace(baseRaw))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return false
	}
	if !strings.EqualFold(stripPort(resolvedHost), base.Hostname()) {
		return false
	}
	_, remotePath, _, ok := providerRemoteParts(remoteRaw)
	return ok && forgejoPathsMatch(base.Path, remotePath)
}

func forgejoPathsMatch(basePath, remotePath string) bool {
	prefix := providerPathParts(basePath)
	remote := providerPathParts(remotePath)
	if len(remote) == len(prefix)+4 && remote[len(remote)-2] == "pulls" {
		number, err := strconv.Atoi(remote[len(remote)-1])
		if err != nil || number <= 0 {
			return false
		}
		remote = remote[:len(remote)-2]
	}
	if len(remote) != len(prefix)+2 {
		return false
	}
	for i := range prefix {
		if remote[i] != prefix[i] {
			return false
		}
	}
	return true
}

func providerRemoteParts(raw string) (host, remotePath, scheme string, ok bool) {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", "", "", false
		}
		return parsed.Host, parsed.Path, strings.ToLower(parsed.Scheme), true
	}
	colon := strings.Index(raw, ":")
	if colon <= 0 {
		return "", "", "", false
	}
	host = raw[:colon]
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	return host, raw[colon+1:], "ssh", host != ""
}

func providerPathParts(raw string) []string {
	raw = strings.Trim(strings.TrimSpace(raw), "/")
	raw = strings.TrimSuffix(raw, ".git")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "/")
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
	case ProviderForgejo:
		return "forgejo-axi"
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
	case ProviderForgejo:
		return []string{"forgejo-axi", "status", "--json"}
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
