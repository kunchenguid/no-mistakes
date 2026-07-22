package forgecontext

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestResolveSelectsGitHubProfileAndBuildsAuthoritativeEnvironment(t *testing.T) {
	dir := t.TempDir()
	hosts := "github.com:\n    users:\n        rudingma:\n    user: rudingma\n"
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte(hosts), 0o644); err != nil {
		t.Fatal(err)
	}
	profiles := config.ForgeProfiles{
		"github.com": {GHConfigDir: dir},
	}

	resolved, err := Resolve(context.Background(), profiles, "https://github.com/rudingma/work-os.git", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved == nil {
		t.Fatal("Resolve returned nil context")
	}
	if resolved.Provider != scm.ProviderGitHub {
		t.Fatalf("provider = %q, want %q", resolved.Provider, scm.ProviderGitHub)
	}
	if resolved.ProfileHost != "github.com" {
		t.Fatalf("profile host = %q, want github.com", resolved.ProfileHost)
	}

	env := envMap(resolved.Environment.Apply([]string{
		"GH_TOKEN=ambient",
		"GITHUB_TOKEN=ambient",
		"GH_HOST=wrong.example.com",
		"GH_REPO=wrong/repo",
		"KEEP=value",
	}))
	if env["GH_CONFIG_DIR"] != dir {
		t.Fatalf("GH_CONFIG_DIR = %q, want %q", env["GH_CONFIG_DIR"], dir)
	}
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_HOST", "GH_REPO"} {
		if _, exists := env[key]; exists {
			t.Fatalf("%s survived selected profile environment", key)
		}
	}
	if env["KEEP"] != "value" {
		t.Fatalf("unrelated environment value = %q, want value", env["KEEP"])
	}
}

func TestResolveSelectsGitLabProfileWithoutSanitizingGitHub(t *testing.T) {
	dir := t.TempDir()
	configYAML := "hosts:\n    gitlab.com:\n        user: matthias78\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	profiles := config.ForgeProfiles{
		"gitlab.com": {GLabConfigDir: dir},
	}

	resolved, err := Resolve(context.Background(), profiles, "https://gitlab.com/almedia/project.git", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved == nil || resolved.Provider != scm.ProviderGitLab {
		t.Fatalf("resolved context = %#v, want GitLab", resolved)
	}

	env := envMap(resolved.Environment.Apply([]string{
		"GITLAB_TOKEN=ambient",
		"GITLAB_ACCESS_TOKEN=ambient",
		"OAUTH_TOKEN=ambient",
		"GITLAB_HOST=https://wrong.example.com",
		"GITLAB_REPO=wrong/repo",
		"GH_TOKEN=keep-github",
	}))
	if env["GLAB_CONFIG_DIR"] != dir {
		t.Fatalf("GLAB_CONFIG_DIR = %q, want %q", env["GLAB_CONFIG_DIR"], dir)
	}
	for _, key := range []string{"GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN", "GITLAB_HOST", "GITLAB_REPO"} {
		if _, exists := env[key]; exists {
			t.Fatalf("%s survived selected profile environment", key)
		}
	}
	if env["GH_TOKEN"] != "keep-github" {
		t.Fatalf("unrelated GitHub token was changed")
	}
}

func TestResolvePreservesAmbientEnvironmentWithoutProfiles(t *testing.T) {
	resolved, err := Resolve(context.Background(), nil, "https://github.com/acme/repo.git", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved != nil {
		t.Fatalf("resolved context = %#v, want nil ambient context", resolved)
	}
}

func TestResolveMatchedProfileDefinesSelfHostedProvider(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte("code.example.test:\n    user: work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(context.Background(), config.ForgeProfiles{
		"code.example.test": {GHConfigDir: dir},
	}, "https://code.example.test/acme/repo.git", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved == nil || resolved.Provider != scm.ProviderGitHub || resolved.Host != "code.example.test" {
		t.Fatalf("resolved context = %#v, want self-hosted GitHub", resolved)
	}
}

func TestResolveActivatesStrictMatchingPerProvider(t *testing.T) {
	githubProfiles := config.ForgeProfiles{
		"github-personal": {GHConfigDir: filepath.Join(t.TempDir(), "gh")},
	}
	if _, err := Resolve(context.Background(), githubProfiles, "https://github.com/acme/repo.git", ""); err == nil {
		t.Fatal("unmatched GitHub repository succeeded after GitHub profile activation")
	}
	if resolved, err := Resolve(context.Background(), githubProfiles, "https://gitlab.com/acme/repo.git", ""); err != nil || resolved != nil {
		t.Fatalf("GitLab repository with only GitHub profiles = (%#v, %v), want ambient nil context", resolved, err)
	}

	gitlabProfiles := config.ForgeProfiles{
		"gitlab-work": {GLabConfigDir: filepath.Join(t.TempDir(), "glab")},
	}
	if resolved, err := Resolve(context.Background(), gitlabProfiles, "https://github.com/acme/repo.git", ""); err != nil || resolved != nil {
		t.Fatalf("GitHub repository with only GitLab profiles = (%#v, %v), want ambient nil context", resolved, err)
	}
	if _, err := Resolve(context.Background(), gitlabProfiles, "https://gitlab.com/acme/repo.git", ""); err == nil {
		t.Fatal("unmatched GitLab repository succeeded after GitLab profile activation")
	}
}

func TestResolveFailsClosedForUnmatchedSelfHostedProvider(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte("code.example.test:\n    user: work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profiles := config.ForgeProfiles{
		"work-code-alias": {GHConfigDir: dir},
	}
	if _, err := Resolve(context.Background(), profiles, "https://code.example.test/acme/repo.git", ""); err == nil {
		t.Fatal("unmatched self-hosted GitHub repository used ambient behavior after GitHub profile activation")
	}
}

func TestResolveUsesForkProfileForGitHubFork(t *testing.T) {
	dir := t.TempDir()
	hosts := "github.com:\n    users:\n        contributor:\n    user: contributor\n"
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte(hosts), 0o644); err != nil {
		t.Fatal(err)
	}
	profiles := config.ForgeProfiles{
		"github-contributor": {GHConfigDir: dir},
	}

	resolved, err := Resolve(
		context.Background(),
		profiles,
		"https://github.com/upstream/project.git",
		"git@github-contributor:contributor/project.git",
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved == nil || resolved.ProfileHost != "github-contributor" {
		t.Fatalf("resolved context = %#v, want fork profile", resolved)
	}
}

func TestResolveUsesForkHostWhenUpstreamHasNoHost(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte("github.com:\n    user: contributor\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resolved, err := Resolve(context.Background(), config.ForgeProfiles{
		"github.com": {GHConfigDir: dir},
	}, "", "https://github.com/contributor/project.git")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved == nil || resolved.Host != "github.com" {
		t.Fatalf("resolved context = %#v, want fork host github.com", resolved)
	}
}

func TestResolveAcceptsLegacySingleAccountGitHubProfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte("github.com:\n    user: legacy-user\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profiles := config.ForgeProfiles{
		"github.com": {GHConfigDir: dir},
	}

	resolved, err := Resolve(context.Background(), profiles, "https://github.com/acme/repo.git", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved == nil || resolved.Provider != scm.ProviderGitHub {
		t.Fatalf("resolved context = %#v, want GitHub", resolved)
	}
}

func TestResolveRejectsProfileWhoseProviderConflictsWithRemote(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("hosts:\n    github.com:\n        user: wrong-provider\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profiles := config.ForgeProfiles{
		"github.com": {GLabConfigDir: dir},
	}

	_, err := Resolve(context.Background(), profiles, "https://github.com/acme/repo.git", "")
	if err == nil {
		t.Fatal("GitLab profile was accepted for an obvious GitHub remote")
	}
}

func TestResolveRejectsGitHubProfileWithMultipleAccounts(t *testing.T) {
	dir := t.TempDir()
	hosts := "github.com:\n    users:\n        personal:\n        work:\n    user: personal\n"
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte(hosts), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Resolve(context.Background(), config.ForgeProfiles{
		"github.com": {GHConfigDir: dir},
	}, "https://github.com/acme/repo.git", "")
	if err == nil {
		t.Fatal("multiple accounts in one GitHub profile were accepted")
	}
}

func TestResolveAcceptsParentAndForkAliasesForSameEffectiveProfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte("github.com:\n    user: contributor\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "gh-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	resolved, err := Resolve(context.Background(), config.ForgeProfiles{
		"github.com":      {GHConfigDir: dir},
		"github-personal": {GHConfigDir: link},
	}, "https://github.com/upstream/project.git", "git@github-personal:contributor/project.git")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved == nil {
		t.Fatal("same effective profile did not resolve")
	}
}

func TestResolveRejectsDifferentParentAndForkProfiles(t *testing.T) {
	_, err := Resolve(context.Background(), config.ForgeProfiles{
		"github.com":      {GHConfigDir: filepath.Join(t.TempDir(), "parent")},
		"github-personal": {GHConfigDir: filepath.Join(t.TempDir(), "fork")},
	}, "https://github.com/upstream/project.git", "git@github-personal:contributor/project.git")
	if err == nil {
		t.Fatal("different parent and fork profiles were accepted")
	}
}

func envMap(env []string) map[string]string {
	result := make(map[string]string, len(env))
	for _, entry := range env {
		for i := 0; i < len(entry); i++ {
			if entry[i] == '=' {
				result[entry[:i]] = entry[i+1:]
				break
			}
		}
	}
	return result
}
