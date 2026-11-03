// Package forgecontext resolves machine-local provider profiles into an
// immutable execution context for one repository run.
package forgecontext

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"gopkg.in/yaml.v3"
)

// ghTokenTimeout bounds the one gh call a gh_user profile makes at run setup,
// so a credential store that never answers fails the run instead of hanging it.
const ghTokenTimeout = 30 * time.Second

// Context is the resolved forge provider and subprocess environment for one
// run. It holds configuration locations and, for a gh_user profile, that
// login's token as an Environment value: it lives only in memory for the run,
// is re-read on recovery, and runenv.Overlay never prints values.
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
	profile, profileHost, ok, err := selectProfile(profiles, remoteFor(upstreamURL), remoteFor(forkURL))
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
	if profile.IsGitHub() {
		profileProvider = scm.ProviderGitHub
	}
	if detected := scm.DetectProviderStaticContext(ctx, targetRemote); detected != scm.ProviderUnknown && detected != profileProvider {
		return nil, fmt.Errorf("forge profile %q selects provider %s but the upstream remote is %s", profileHost, profileProvider, detected)
	}
	if profile.IsGitHub() {
		var env runenv.Overlay
		if profile.GHUser != "" {
			env, err = githubUserEnvironment(ctx, targetHost, profile.GHUser)
		} else {
			err = validateGitHubProfile(profile.GHConfigDir, targetHost, profile.ExpectedLogin)
			env = githubEnvironment(profile.GHConfigDir)
		}
		if err != nil {
			return nil, fmt.Errorf("forge profile %q: %w", profileHost, err)
		}
		return &Context{
			Provider:    scm.ProviderGitHub,
			ProfileHost: profileHost,
			Host:        targetHost,
			ConfigDir:   profile.GHConfigDir,
			Environment: env,
		}, nil
	}
	if err := validateGitLabProfile(profile.GLabConfigDir, targetHost, profile.ExpectedLogin); err != nil {
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

// RefuseProviderPluginOverlap fails a run whose selected forge profile and a
// provider plugin both claim its repository. Load-time validation compares
// only literal host tokens, so it cannot see a profile keyed by an SSH alias
// while a plugin claims the alias's resolved HostName (or the reverse); here
// the run's remote is known, and the plugin is selected exactly as the PR and
// CI steps select it. Letting either side win would split one repository
// between two provider implementations.
func RefuseProviderPluginOverlap(ctx context.Context, forge *Context, plugins config.ProviderPlugins, upstreamURL string) error {
	upstreamURL = strings.TrimSpace(upstreamURL)
	return refuseProviderPluginOverlap(forge, plugins, scm.ExtractHost(upstreamURL), func() string { return scm.ResolveHost(ctx, upstreamURL) })
}

func refuseProviderPluginOverlap(forge *Context, plugins config.ProviderPlugins, rawHost string, resolvedHost func() string) error {
	if forge == nil || len(plugins) == 0 || rawHost == "" {
		return nil
	}
	name, ok := plugins.Select(rawHost, resolvedHost)
	if !ok {
		return nil
	}
	return fmt.Errorf("forge profile %q and provider_plugins.%s both claim the repository's host; remove one so a single provider serves it", forge.ProfileHost, name)
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
	User string `yaml:"user"`
	// Users decodes account keys only. gh nests each account's oauth_token
	// under its login, and only the logins are ever read, so an empty-struct
	// value keeps a live credential out of the process image entirely.
	Users map[string]struct{} `yaml:"users"`
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
	User    string `yaml:"user"`
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

// remote is the part of a repository remote that selects a forge profile: its
// raw host token and its repository owner, both lowercased.
type remote struct {
	host  string
	owner string
}

func remoteFor(remoteURL string) remote {
	host := scm.ExtractHost(remoteURL)
	if host == "" {
		return remote{}
	}
	owner, _, _ := strings.Cut(scm.RepoPath(remoteURL), "/")
	return remote{host: host, owner: strings.ToLower(strings.TrimSpace(owner))}
}

// selectProfile picks the profile for a repository's parent and fork remotes.
// An owner-scoped key ("host/owner") is more specific than its plain host key,
// so it wins when the other side matched only a host key: that is how one
// account on a shared host is chosen for the repositories its owner holds.
// Two matches at the same specificity must still select the same account.
func selectProfile(profiles config.ForgeProfiles, upstream, fork remote) (config.ForgeProfile, string, bool, error) {
	upstreamProfile, upstreamName, upstreamScoped, upstreamOK := profileForRemote(profiles, upstream)
	forkProfile, forkName, forkScoped, forkOK := profileForRemote(profiles, fork)
	switch {
	case upstreamOK && forkOK:
		if sameProfile(upstreamProfile, forkProfile) {
			return upstreamProfile, upstreamName, true, nil
		}
		if upstreamScoped && !forkScoped {
			return upstreamProfile, upstreamName, true, nil
		}
		if forkScoped && !upstreamScoped {
			return forkProfile, forkName, true, nil
		}
		return config.ForgeProfile{}, "", false, fmt.Errorf("forge profiles %q for the parent and %q for the fork are ambiguous", upstreamName, forkName)
	case upstreamOK:
		return upstreamProfile, upstreamName, true, nil
	case forkOK:
		return forkProfile, forkName, true, nil
	default:
		return config.ForgeProfile{}, "", false, nil
	}
}

// sameProfile reports whether two host tokens resolve to the same account
// selection. The expected_login pin is part of that identity: two profiles
// sharing a config directory but pinning different logins disagree about which
// account the run must be signed in as, and silently taking either one is the
// fallback expectLogin exists to refuse.
func sameProfile(a, b config.ForgeProfile) bool {
	return effectiveProfilePath(a.GHConfigDir) == effectiveProfilePath(b.GHConfigDir) &&
		effectiveProfilePath(a.GLabConfigDir) == effectiveProfilePath(b.GLabConfigDir) &&
		normalizeExpectedLogin(a.ExpectedLogin) == normalizeExpectedLogin(b.ExpectedLogin) &&
		normalizeExpectedLogin(a.GHUser) == normalizeExpectedLogin(b.GHUser)
}

func normalizeExpectedLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

func effectiveProfilePath(path string) string {
	cleaned := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err == nil {
		return resolved
	}
	return cleaned
}

// profilesActivateProvider reports whether a plain host profile makes routing
// mandatory for provider. An owner-scoped key speaks only for its owner, so it
// never forces a profile onto repositories other owners hold.
func profilesActivateProvider(profiles config.ForgeProfiles, provider scm.Provider) bool {
	for key, profile := range profiles {
		if _, owner := config.SplitForgeProfileKey(key); owner != "" {
			continue
		}
		switch provider {
		case scm.ProviderGitHub:
			if profile.IsGitHub() {
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

// githubEnvironment pins GH_CONFIG_DIR when dir is set and removes every
// ambient variable that would outrank the selected account.
func githubEnvironment(dir string) runenv.Overlay {
	set := map[string]string{}
	if dir != "" {
		set["GH_CONFIG_DIR"] = dir
	}
	return runenv.Overlay{
		Set: set,
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

// profileForRemote looks up the owner-scoped key first and falls back to the
// plain host key, so existing host-keyed configurations keep matching.
func profileForRemote(profiles config.ForgeProfiles, r remote) (profile config.ForgeProfile, name string, scoped, ok bool) {
	if r.host == "" {
		return config.ForgeProfile{}, "", false, false
	}
	if r.owner != "" {
		if profile, name, ok := profileForKey(profiles, r.host+"/"+r.owner); ok {
			return profile, name, true, true
		}
	}
	profile, name, ok = profileForKey(profiles, r.host)
	return profile, name, false, ok
}

func profileForKey(profiles config.ForgeProfiles, key string) (config.ForgeProfile, string, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	for candidate, profile := range profiles {
		if strings.EqualFold(strings.TrimSpace(candidate), key) {
			return profile, strings.ToLower(strings.TrimSpace(candidate)), true
		}
	}
	return config.ForgeProfile{}, "", false
}

// githubUserEnvironment reads the stored token of one login signed in to the
// ambient gh configuration and returns the
// run environment that authenticates every gh and gh-credential-helper call as
// that login. The token is read once, from gh's stdout only; it never reaches
// an argument, a log line, a file, or an error. A login gh cannot produce a
// token for refuses the run instead of falling back to the active account.
func githubUserEnvironment(ctx context.Context, targetHost, login string) (runenv.Overlay, error) {
	env := githubEnvironment("")
	ctx, cancel := context.WithTimeout(ctx, ghTokenTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", targetHost, "--user", login)
	cmd.Env = env.Apply(nil)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	shellenv.ConfigureShellCommand(cmd)
	out, err := shellenv.OutputShellCommand(cmd)
	if err != nil {
		detail := firstLine(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return runenv.Overlay{}, fmt.Errorf("gh_user %q has no token for GitHub host %q (%s); sign it in with gh auth login, it is never replaced by the active account", login, targetHost, detail)
	}
	token := strings.TrimSpace(string(out))
	if token == "" || strings.IndexFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return runenv.Overlay{}, fmt.Errorf("gh_user %q: gh auth token returned no usable token for GitHub host %q", login, targetHost)
	}
	env.Set[githubTokenVariable(targetHost)] = token
	return env, nil
}

// githubTokenVariable names the variable gh reads a token from for host:
// GH_TOKEN for github.com and GHE.com tenancies, GH_ENTERPRISE_TOKEN for
// GitHub Enterprise Server hosts.
func githubTokenVariable(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "github.com" || strings.HasSuffix(host, ".ghe.com") {
		return "GH_TOKEN"
	}
	return "GH_ENTERPRISE_TOKEN"
}

// firstLine bounds a subprocess diagnostic to one short line.
func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.TrimSpace(line)
	if runes := []rune(line); len(runes) > 200 {
		line = string(runes[:200]) + "..."
	}
	return line
}

func requireDirectory(dir, provider string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("%s config directory: %w", provider, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s config directory %q is not a directory", provider, dir)
	}
	return nil
}

func validateGitHubProfile(dir, targetHost, expectedLogin string) error {
	if err := requireDirectory(dir, "GitHub"); err != nil {
		return err
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
		return expectLogin(expectedLogin, entry.User, "GitHub", targetHost)
	}
	if len(entry.Users) != 1 {
		return fmt.Errorf("GitHub host %q must contain exactly one account", targetHost)
	}
	for login := range entry.Users {
		if entry.User == "" || !strings.EqualFold(strings.TrimSpace(entry.User), strings.TrimSpace(login)) {
			return fmt.Errorf("GitHub host %q must have its only account active", targetHost)
		}
	}
	return expectLogin(expectedLogin, entry.User, "GitHub", targetHost)
}

// expectLogin fails closed when a profile pins expected_login and the
// provider config's active account differs. An empty expectation keeps the
// existing behavior; an empty active login under a pinned expectation is a
// mismatch, never a fallback.
func expectLogin(expected, active, provider, targetHost string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return nil
	}
	active = strings.TrimSpace(active)
	if !strings.EqualFold(active, expected) {
		return fmt.Errorf("%s host %q is signed in as %q, but the profile expects login %q", provider, targetHost, active, expected)
	}
	return nil
}

func validateGitLabProfile(dir, targetHost, expectedLogin string) error {
	if err := requireDirectory(dir, "GitLab"); err != nil {
		return err
	}
	cfg, err := loadGitLabConfig(dir)
	if err != nil {
		return err
	}
	if !gitlabConfigHasHost(cfg, targetHost) {
		return fmt.Errorf("GitLab host %q is not configured", targetHost)
	}
	return expectLogin(expectedLogin, gitlabActiveUser(cfg, targetHost), "GitLab", targetHost)
}

func gitlabActiveUser(cfg gitlabConfig, targetHost string) string {
	for host, entry := range cfg.Hosts {
		if strings.EqualFold(strings.TrimSpace(host), targetHost) || strings.EqualFold(scm.ExtractHost(entry.APIHost), targetHost) {
			return entry.User
		}
	}
	return ""
}
