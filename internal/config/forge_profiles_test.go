package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadGlobalParsesProviderSpecificForgeProfiles(t *testing.T) {
	githubDir := filepath.Join(t.TempDir(), "gh-personal")
	gitlabDir := filepath.Join(t.TempDir(), "glab-work")
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "forge_profiles:\n" +
		"  GitHub-Personal:\n" +
		"    gh_config_dir: " + githubDir + "\n" +
		"  gitlab-work:\n" +
		"    glab_config_dir: " + gitlabDir + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}

	if got := cfg.ForgeProfiles["github-personal"].GHConfigDir; got != githubDir {
		t.Fatalf("GitHub config dir = %q, want %q", got, githubDir)
	}
	if got := cfg.ForgeProfiles["gitlab-work"].GLabConfigDir; got != gitlabDir {
		t.Fatalf("GitLab config dir = %q, want %q", got, gitlabDir)
	}
}

func TestLoadGlobalPreservesAbsentAndEmptyForgeProfiles(t *testing.T) {
	for _, contents := range []string{"agent: auto\n", "forge_profiles: {}\n"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadGlobal(path)
		if err != nil {
			t.Fatalf("LoadGlobal(%q): %v", contents, err)
		}
		if len(cfg.ForgeProfiles) != 0 {
			t.Fatalf("ForgeProfiles = %#v, want empty", cfg.ForgeProfiles)
		}
	}
}

func TestLoadGlobalRejectsUnknownForgeProfileField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "forge_profiles:\n  github.com:\n    gh_config_dir: /tmp/gh\n    token: forbidden\n"
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGlobal(path); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("LoadGlobal error = %v, want unknown-field error", err)
	}
}

func TestLoadGlobalRejectsForgeProfileWithoutExactlyOneProvider(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile string
	}{
		{name: "neither", profile: "{}"},
		{name: "both", profile: "{gh_config_dir: /tmp/gh, glab_config_dir: /tmp/glab}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			contents := "forge_profiles:\n  github-personal: " + tc.profile + "\n"
			if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := LoadGlobal(path)
			if err == nil || !strings.Contains(err.Error(), "exactly one") {
				t.Fatalf("LoadGlobal error = %v, want exactly-one-provider error", err)
			}
		})
	}
}

func TestLoadGlobalExpandsHomeRelativeForgeProfilePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("forge_profiles:\n  github-personal:\n    gh_config_dir: ~/profiles/gh-personal\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if got, want := cfg.ForgeProfiles["github-personal"].GHConfigDir, filepath.Join(home, "profiles", "gh-personal"); got != want {
		t.Fatalf("GitHub config dir = %q, want %q", got, want)
	}
}

func TestLoadGlobalRejectsNonAbsoluteForgeProfilePath(t *testing.T) {
	for _, value := range []string{"profiles/gh", "$HOME/profiles/gh", "${HOME}/profiles/gh", "~someone/profiles/gh"} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			contents := "forge_profiles:\n  github-personal:\n    gh_config_dir: " + value + "\n"
			if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := LoadGlobal(path)
			if err == nil || !strings.Contains(err.Error(), "absolute or start with ~/") {
				t.Fatalf("LoadGlobal error = %v, want absolute-path error", err)
			}
		})
	}
}

func TestLoadGlobalRejectsCaseInsensitiveDuplicateForgeHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	one := filepath.Join(t.TempDir(), "one")
	two := filepath.Join(t.TempDir(), "two")
	contents := "forge_profiles:\n" +
		"  GitHub-Personal:\n" +
		"    gh_config_dir: " + one + "\n" +
		"  github-personal:\n" +
		"    gh_config_dir: " + two + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadGlobal(path)
	if err == nil || !strings.Contains(err.Error(), "duplicate host") {
		t.Fatalf("LoadGlobal error = %v, want duplicate-host error", err)
	}
}

func TestLoadGlobalRejectsEmptyForgeHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("forge_profiles:\n  '':\n    gh_config_dir: /tmp/gh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadGlobal(path)
	if err == nil || !strings.Contains(err.Error(), "host must not be empty") {
		t.Fatalf("LoadGlobal error = %v, want empty-host error", err)
	}
}

func TestLoadGlobalParsesOwnerScopedGHUserProfiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gh-work")
	cfg, err := LoadGlobalFromBytes([]byte("forge_profiles:\n" +
		"  GitHub.com/RemiCaesar:\n" +
		"    gh_user: ' remicaesar '\n" +
		"  github.com:\n" +
		"    gh_config_dir: " + dir + "\n"))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	if got := cfg.ForgeProfiles["github.com/remicaesar"]; got.GHUser != "remicaesar" || got.GHConfigDir != "" {
		t.Fatalf("owner-scoped ambient profile = %#v", got)
	}
	if got := cfg.ForgeProfiles["github.com"]; got.GHUser != "" || got.GHConfigDir != dir {
		t.Fatalf("plain host profile changed = %#v", got)
	}
}

func TestLoadGlobalRejectsInvalidGHUserProfiles(t *testing.T) {
	for _, tc := range []struct {
		name    string
		key     string
		profile string
		want    string
	}{
		{name: "with glab dir", key: "gitlab.com", profile: "{gh_user: me, glab_config_dir: /tmp/glab}", want: "cannot be combined with glab_config_dir"},
		{name: "with expected login", key: "github.com", profile: "{gh_user: me, expected_login: me}", want: "remove expected_login"},
		{name: "flag-shaped login", key: "github.com", profile: "{gh_user: --hostname}", want: "is not a GitHub login"},
		{name: "login with space", key: "github.com", profile: "{gh_user: 'a b'}", want: "is not a GitHub login"},
		{name: "with gh config dir", key: "github.com/acme", profile: "{gh_user: me, gh_config_dir: /tmp/gh}", want: "cannot be combined with gh_config_dir"},
		{name: "owner-scoped gitlab", key: "gitlab.com/group", profile: "{glab_config_dir: /tmp/glab}", want: "only for gh_user profiles"},
		{name: "owner-scoped gh config dir", key: "github.com/acme", profile: "{gh_config_dir: /tmp/gh}", want: "only for gh_user profiles"},
		{name: "owner-scoped expected login", key: "github.com/acme", profile: "{gh_config_dir: /tmp/gh, expected_login: acme-bot}", want: "only for gh_user profiles"},
		{name: "nested owner", key: "github.com/a/b", profile: "{gh_user: me}", want: "single owner segment"},
		{name: "empty owner", key: "github.com/", profile: "{gh_user: me}", want: "single owner segment"},
		{name: "empty host", key: "/acme", profile: "{gh_user: me}", want: "host must not be empty"},
		{name: "token field", key: "github.com/acme", profile: "{gh_user: me, token: secret}", want: "token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadGlobalFromBytes([]byte("forge_profiles:\n  '" + tc.key + "': " + tc.profile + "\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadGlobalFromBytes error = %v, want %q", err, tc.want)
			}
		})
	}
}

// The owner-to-login mapping decides which account's credentials a run
// executes with, so nothing on the pushed branch may supply or extend it.
func TestMergeTakesForgeProfilesFromGlobalOnly(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("forge_profiles:\n  github.com/acme:\n    gh_user: acme-bot\n"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := LoadRepoFromBytes([]byte("forge_profiles:\n  github.com/acme:\n    gh_user: attacker\n  github.com/evil:\n    gh_user: attacker\n"))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes: %v", err)
	}
	merged := Merge(global, repo)
	if len(merged.ForgeProfiles) != 1 || merged.ForgeProfiles["github.com/acme"].GHUser != "acme-bot" {
		t.Fatalf("merged ForgeProfiles = %#v", merged.ForgeProfiles)
	}
	if repoOnly := Merge(DefaultGlobalConfig(), repo); len(repoOnly.ForgeProfiles) != 0 {
		t.Fatalf("repository config configured a forge profile: %#v", repoOnly.ForgeProfiles)
	}
}

func TestLoadGlobalRejectsOwnerScopedProfileHostClaimedByPlugin(t *testing.T) {
	_, err := LoadGlobalFromBytes([]byte("forge_profiles:\n  git.example.com/acme:\n    gh_user: acme-bot\n" +
		"provider_plugins:\n  ssm:\n    command: nm-ssm\n    hosts: [git.example.com]\n"))
	if err == nil || !strings.Contains(err.Error(), "also claimed by provider_plugins.ssm") {
		t.Fatalf("LoadGlobalFromBytes error = %v, want plugin overlap", err)
	}
}
