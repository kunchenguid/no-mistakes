// Package forgecontext resolves machine-local provider profiles into an
// immutable execution context for one repository run.
package forgecontext

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"gopkg.in/yaml.v3"
)

// Context is the resolved forge provider and subprocess environment for one
// run. It contains configuration locations, never credentials.
type Context struct {
	Provider    scm.Provider
	ProfileHost string
	Host        string
	ConfigDir   string
	Environment runenv.Overlay
}

// Resolve selects and validates the profile associated with a repository's
// configured remotes. A nil result means profile routing is inactive for the
// repository and callers must preserve ambient behavior.
func Resolve(ctx context.Context, profiles config.ForgeProfiles, upstreamURL, forkURL string) (*Context, error) {
	if len(profiles) == 0 {
		return nil, nil
	}
	upstreamHost := scm.ExtractHost(upstreamURL)
	forkHost := scm.ExtractHost(forkURL)
	profile, profileHost, ok, err := selectProfile(profiles, upstreamHost, forkHost)
	if err != nil {
		return nil, err
	}
	if !ok {
		provider := scm.DetectProviderStaticContext(ctx, upstreamURL)
		targetRemote := upstreamURL
		if strings.TrimSpace(targetRemote) == "" {
			targetRemote = forkURL
		}
		if provider == scm.ProviderUnknown {
			var inferErr error
			provider, inferErr = configuredProviderForHost(profiles, scm.ResolveHost(ctx, targetRemote))
			if inferErr != nil {
				return nil, inferErr
			}
		}
		if provider == scm.ProviderUnknown {
			provider = scm.DetectProviderContext(ctx, upstreamURL)
			if provider == scm.ProviderUnknown && strings.TrimSpace(forkURL) != "" {
				provider = scm.DetectProviderContext(ctx, forkURL)
			}
		}
		if profilesActivateProvider(profiles, provider) {
			return nil, fmt.Errorf("no forge profile matches repository hosts %q and %q for activated provider %s", upstreamHost, forkHost, provider)
		}
		return nil, nil
	}
	targetRemote := upstreamURL
	if upstreamHost == "" {
		targetRemote = forkURL
	}
	targetHost := scm.ResolveHost(ctx, targetRemote)
	profileProvider := scm.ProviderGitLab
	if profile.GHConfigDir != "" {
		profileProvider = scm.ProviderGitHub
	}
	if detected := scm.DetectProviderStaticContext(ctx, targetRemote); detected != scm.ProviderUnknown && detected != profileProvider {
		return nil, fmt.Errorf("forge profile %q selects provider %s but the upstream remote is %s", profileHost, profileProvider, detected)
	}
	if profile.GHConfigDir != "" {
		if err := validateGitHubProfile(profile.GHConfigDir, targetHost); err != nil {
			return nil, fmt.Errorf("forge profile %q: %w", profileHost, err)
		}
		return &Context{
			Provider:    scm.ProviderGitHub,
			ProfileHost: profileHost,
			Host:        targetHost,
			ConfigDir:   profile.GHConfigDir,
			Environment: githubEnvironment(profile.GHConfigDir),
		}, nil
	}
	if err := validateGitLabProfile(profile.GLabConfigDir, targetHost); err != nil {
		return nil, fmt.Errorf("forge profile %q: %w", profileHost, err)
	}
	return &Context{
		Provider:    scm.ProviderGitLab,
		ProfileHost: profileHost,
		Host:        targetHost,
		ConfigDir:   profile.GLabConfigDir,
		Environment: gitlabEnvironment(profile.GLabConfigDir),
	}, nil
}

func configuredProviderForHost(profiles config.ForgeProfiles, targetHost string) (scm.Provider, error) {
	if targetHost == "" {
		return scm.ProviderUnknown, nil
	}
	githubMatch := false
	gitlabMatch := false
	for _, profile := range profiles {
		if profile.GHConfigDir != "" && githubConfigContainsHost(profile.GHConfigDir, targetHost) {
			githubMatch = true
		}
		if profile.GLabConfigDir != "" && gitlabConfigContainsHost(profile.GLabConfigDir, targetHost) {
			gitlabMatch = true
		}
	}
	if githubMatch && gitlabMatch {
		return scm.ProviderUnknown, fmt.Errorf("repository host %q appears in both GitHub and GitLab forge profiles", targetHost)
	}
	if githubMatch {
		return scm.ProviderGitHub, nil
	}
	if gitlabMatch {
		return scm.ProviderGitLab, nil
	}
	return scm.ProviderUnknown, nil
}

func githubConfigContainsHost(dir, targetHost string) bool {
	hosts, err := loadGitHubHosts(dir)
	if err != nil {
		return false
	}
	_, ok := githubHostEntryFor(hosts, targetHost)
	return ok
}

func gitlabConfigContainsHost(dir, targetHost string) bool {
	cfg, err := loadGitLabConfig(dir)
	if err != nil {
		return false
	}
	return gitlabConfigHasHost(cfg, targetHost)
}

type githubHostEntry struct {
	User  string                 `yaml:"user"`
	Users map[string]interface{} `yaml:"users"`
}

type githubHosts map[string]githubHostEntry

func loadGitHubHosts(dir string) (githubHosts, error) {
	data, err := os.ReadFile(filepath.Join(dir, "hosts.yml"))
	if err != nil {
		return nil, fmt.Errorf("read GitHub hosts: %w", err)
	}
	var hosts githubHosts
	if err := yaml.Unmarshal(data, &hosts); err != nil {
		return nil, fmt.Errorf("parse GitHub hosts: %w", err)
	}
	return hosts, nil
}

func githubHostEntryFor(hosts githubHosts, targetHost string) (githubHostEntry, bool) {
	for host, entry := range hosts {
		if strings.EqualFold(strings.TrimSpace(host), targetHost) {
			return entry, true
		}
	}
	return githubHostEntry{}, false
}

type gitlabHostEntry struct {
	APIHost string `yaml:"api_host"`
}

type gitlabConfig struct {
	Hosts map[string]gitlabHostEntry `yaml:"hosts"`
}

func loadGitLabConfig(dir string) (gitlabConfig, error) {
	data, err := os.ReadFile(filepath.Join(dir, "config.yml"))
	if err != nil {
		return gitlabConfig{}, fmt.Errorf("read GitLab config: %w", err)
	}
	var cfg gitlabConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return gitlabConfig{}, fmt.Errorf("parse GitLab config: %w", err)
	}
	return cfg, nil
}

func gitlabConfigHasHost(cfg gitlabConfig, targetHost string) bool {
	for host, entry := range cfg.Hosts {
		if strings.EqualFold(strings.TrimSpace(host), targetHost) || strings.EqualFold(scm.ExtractHost(entry.APIHost), targetHost) {
			return true
		}
	}
	return false
}

func selectProfile(profiles config.ForgeProfiles, upstreamHost, forkHost string) (config.ForgeProfile, string, bool, error) {
	upstreamProfile, upstreamName, upstreamOK := profileForHost(profiles, upstreamHost)
	forkProfile, forkName, forkOK := profileForHost(profiles, forkHost)
	switch {
	case upstreamOK && forkOK:
		if !sameProfile(upstreamProfile, forkProfile) {
			return config.ForgeProfile{}, "", false, fmt.Errorf("forge profiles for parent host %q and fork host %q are ambiguous", upstreamHost, forkHost)
		}
		return upstreamProfile, upstreamName, true, nil
	case upstreamOK:
		return upstreamProfile, upstreamName, true, nil
	case forkOK:
		return forkProfile, forkName, true, nil
	default:
		return config.ForgeProfile{}, "", false, nil
	}
}

func sameProfile(a, b config.ForgeProfile) bool {
	return effectiveProfilePath(a.GHConfigDir) == effectiveProfilePath(b.GHConfigDir) &&
		effectiveProfilePath(a.GLabConfigDir) == effectiveProfilePath(b.GLabConfigDir)
}

func effectiveProfilePath(path string) string {
	cleaned := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err == nil {
		return resolved
	}
	return cleaned
}

func profilesActivateProvider(profiles config.ForgeProfiles, provider scm.Provider) bool {
	for _, profile := range profiles {
		switch provider {
		case scm.ProviderGitHub:
			if profile.GHConfigDir != "" {
				return true
			}
		case scm.ProviderGitLab:
			if profile.GLabConfigDir != "" {
				return true
			}
		}
	}
	return false
}

func githubEnvironment(dir string) runenv.Overlay {
	return runenv.Overlay{
		Set: map[string]string{"GH_CONFIG_DIR": dir},
		Unset: []string{
			"GH_TOKEN",
			"GITHUB_TOKEN",
			"GH_ENTERPRISE_TOKEN",
			"GITHUB_ENTERPRISE_TOKEN",
			"GH_HOST",
			"GH_REPO",
		},
	}
}

func gitlabEnvironment(dir string) runenv.Overlay {
	return runenv.Overlay{
		Set: map[string]string{"GLAB_CONFIG_DIR": dir},
		Unset: []string{
			"GITLAB_TOKEN",
			"GITLAB_ACCESS_TOKEN",
			"OAUTH_TOKEN",
			"CI_JOB_TOKEN",
			"GLAB_ENABLE_CI_AUTOLOGIN",
			"GITLAB_HOST",
			"GL_HOST",
			"GITLAB_URI",
			"GITLAB_API_HOST",
			"GITLAB_REPO",
			"GITLAB_GROUP",
			"REMOTE_ALIAS",
			"GIT_REMOTE_URL_VAR",
		},
	}
}

func profileForHost(profiles config.ForgeProfiles, host string) (config.ForgeProfile, string, bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	for candidate, profile := range profiles {
		if strings.EqualFold(strings.TrimSpace(candidate), host) {
			return profile, strings.ToLower(strings.TrimSpace(candidate)), true
		}
	}
	return config.ForgeProfile{}, "", false
}

func validateGitHubProfile(dir, targetHost string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("GitHub config directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("GitHub config directory %q is not a directory", dir)
	}
	hosts, err := loadGitHubHosts(dir)
	if err != nil {
		return err
	}
	entry, ok := githubHostEntryFor(hosts, targetHost)
	if !ok {
		return fmt.Errorf("GitHub host %q is not configured", targetHost)
	}
	if len(entry.Users) == 0 && strings.TrimSpace(entry.User) != "" {
		return nil
	}
	if len(entry.Users) != 1 {
		return fmt.Errorf("GitHub host %q must contain exactly one account", targetHost)
	}
	for login := range entry.Users {
		if entry.User == "" || !strings.EqualFold(strings.TrimSpace(entry.User), strings.TrimSpace(login)) {
			return fmt.Errorf("GitHub host %q must have its only account active", targetHost)
		}
	}
	return nil
}

func validateGitLabProfile(dir, targetHost string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("GitLab config directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("GitLab config directory %q is not a directory", dir)
	}
	cfg, err := loadGitLabConfig(dir)
	if err != nil {
		return err
	}
	if !gitlabConfigHasHost(cfg, targetHost) {
		return fmt.Errorf("GitLab host %q is not configured", targetHost)
	}
	return nil
}
