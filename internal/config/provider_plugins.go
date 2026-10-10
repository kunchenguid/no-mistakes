package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// DefaultProviderPluginTimeout bounds one provider-plugin invocation when the
// operator did not set provider_plugins.<name>.timeout.
const DefaultProviderPluginTimeout = 2 * time.Minute

var providerPluginNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ProviderPlugin is one resolved provider_plugins entry: an executable that
// implements PR and CI operations for repositories on the listed hosts.
//
// It is global-only by construction. The command executes with the
// operator's credentials for every repository on those hosts, so - like
// agent_args_override - no repository file may add, change, or redirect it.
// RepoConfig has no such field, so a provider_plugins block in a repository
// .no-mistakes.yaml is ignored.
type ProviderPlugin struct {
	Command           string
	Args              []string
	Hosts             []string
	Timeout           time.Duration
	DraftPullRequests bool
}

// ProviderPlugins maps a plugin name to its resolved configuration.
type ProviderPlugins map[string]ProviderPlugin

type providerPluginRaw struct {
	Command           string   `yaml:"command"`
	Args              []string `yaml:"args"`
	Hosts             []string `yaml:"hosts"`
	Timeout           string   `yaml:"timeout"`
	DraftPullRequests bool     `yaml:"draft_pull_requests"`
}

func normalizeProviderPlugins(raw map[string]providerPluginRaw) (ProviderPlugins, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	plugins := make(ProviderPlugins, len(raw))
	owners := map[string]string{}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := raw[name]
		if !providerPluginNamePattern.MatchString(name) {
			return nil, fmt.Errorf("invalid provider_plugins.%s: name must be 1-63 lowercase letters, digits, '-' or '_', starting with a letter or digit", name)
		}
		for _, builtin := range scm.BuiltinProviders {
			if name == string(builtin) {
				return nil, fmt.Errorf("invalid provider_plugins.%s: %q is a built-in provider name", name, name)
			}
		}
		command, err := normalizeProviderPluginCommand(entry.Command)
		if err != nil {
			return nil, fmt.Errorf("invalid provider_plugins.%s.command: %w", name, err)
		}
		if len(entry.Hosts) == 0 {
			return nil, fmt.Errorf("invalid provider_plugins.%s.hosts: at least one host is required", name)
		}
		hosts := make([]string, 0, len(entry.Hosts))
		for _, rawHost := range entry.Hosts {
			host, err := normalizeProviderPluginHost(rawHost)
			if err != nil {
				return nil, fmt.Errorf("invalid provider_plugins.%s.hosts: %w", name, err)
			}
			if owner, taken := owners[host]; taken {
				return nil, fmt.Errorf("invalid provider_plugins.%s.hosts: %q is already claimed by provider_plugins.%s", name, host, owner)
			}
			owners[host] = name
			hosts = append(hosts, host)
		}
		timeout := DefaultProviderPluginTimeout
		if strings.TrimSpace(entry.Timeout) != "" {
			timeout, err = parsePositiveDuration("provider_plugins."+name+".timeout", strings.TrimSpace(entry.Timeout))
			if err != nil {
				return nil, err
			}
		}
		plugins[name] = ProviderPlugin{
			Command:           command,
			Args:              append([]string(nil), entry.Args...),
			Hosts:             hosts,
			Timeout:           timeout,
			DraftPullRequests: entry.DraftPullRequests,
		}
	}
	return plugins, nil
}

// normalizeProviderPluginCommand accepts a bare executable name (resolved
// from the run's PATH) or an absolute or ~/ path. A relative path is refused:
// provider commands run with the run worktree as their working directory, so
// a relative path would execute a file the pushed branch controls.
func normalizeProviderPluginCommand(raw string) (string, error) {
	command := strings.TrimSpace(raw)
	if command == "" {
		return "", fmt.Errorf("command is required")
	}
	if !strings.ContainsAny(command, `/\`) {
		return command, nil
	}
	return normalizeForgeProfilePath(command)
}

// normalizeProviderPluginHost validates one host pattern: a host name (or SSH
// alias) exactly as it appears in a remote URL, or "*.<suffix>" for any
// subdomain of suffix. Ports, schemes, paths, and user info are refused
// because remote host extraction strips all of them.
func normalizeProviderPluginHost(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(raw))
	if host == "" {
		return "", fmt.Errorf("host must not be empty")
	}
	suffix := strings.TrimPrefix(host, "*.")
	if suffix == "" || strings.ContainsAny(suffix, "*/:@ \t") || strings.HasPrefix(suffix, ".") || strings.HasSuffix(suffix, ".") || strings.Contains(suffix, "..") {
		return "", fmt.Errorf("%q is not a host name or *.<domain> pattern (no scheme, port, path, or user)", raw)
	}
	return host, nil
}

// Select returns the plugin that claims a repository remote. rawHost is the
// literal host token in the remote (an SSH alias for alias remotes) and
// resolvedHost its SSH HostName resolution; resolvedHost may be computed
// lazily and is consulted only when rawHost matches nothing, so an alias the
// operator chose deliberately always decides first. Within one host an exact
// pattern beats a wildcard and a longer wildcard suffix beats a shorter one;
// load-time validation forbids identical patterns, so the result is unique.
func (p ProviderPlugins) Select(rawHost string, resolvedHost func() string) (string, bool) {
	if len(p) == 0 {
		return "", false
	}
	if name, ok := p.selectHost(rawHost); ok {
		return name, true
	}
	if resolvedHost == nil {
		return "", false
	}
	if resolved := resolvedHost(); resolved != "" && !strings.EqualFold(resolved, rawHost) {
		return p.selectHost(resolved)
	}
	return "", false
}

func (p ProviderPlugins) selectHost(host string) (string, bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return "", false
	}
	best, bestLen, exact := "", -1, false
	for name, plugin := range p {
		for _, pattern := range plugin.Hosts {
			switch {
			case pattern == host:
				if !exact {
					best, bestLen, exact = name, len(pattern), true
				}
			case !exact && strings.HasPrefix(pattern, "*.") && strings.HasSuffix(host, pattern[1:]) && len(pattern) > bestLen:
				best, bestLen = name, len(pattern)
			}
		}
	}
	return best, bestLen >= 0
}

// validateProviderPluginsAgainstForgeProfiles refuses a host token that both a
// forge profile and a provider plugin would claim. The two features route a
// repository to different implementations, and silently letting one win would
// hide a misconfiguration behind whichever ran first. It sees only literal
// host tokens; forgecontext.RefuseProviderPluginOverlap catches the overlap
// that appears only after SSH HostName resolution, when a run starts.
func validateProviderPluginsAgainstForgeProfiles(plugins ProviderPlugins, profiles ForgeProfiles) error {
	if len(plugins) == 0 || len(profiles) == 0 {
		return nil
	}
	hosts := make([]string, 0, len(profiles))
	for host := range profiles {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		if name, ok := plugins.selectHost(host); ok {
			return fmt.Errorf("invalid forge_profiles.%s: host is also claimed by provider_plugins.%s", host, name)
		}
	}
	return nil
}
