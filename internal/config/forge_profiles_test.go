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
