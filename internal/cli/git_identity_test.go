package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/spf13/cobra"
)

// These tests need no daemon, remote, credentials, or ambient Git config.
func setupIdentityRepo(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") || key == "EMAIL" {
			t.Setenv(key, "") // Register restoration before unsetting.
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	home := t.TempDir()
	// Keep scratch directories outside any enclosing repository even when
	// TMPDIR is inside a worktree (as in sandboxed pipeline validation).
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(home))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "absent-global"))
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(realGit)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(t.TempDir())
	identityGit(t, "init", "--template=")
	identityGit(t, "config", "user.useConfigOnly", "true")
}

func identityGit(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func identityConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckGitCommitIdentity(t *testing.T) {
	valid := []string{"user.name=Identity Fixture", "user.email=fixture@example.invalid"}
	roles := []string{"author.name=Author Fixture", "author.email=author@example.invalid", "committer.name=Committer Fixture", "committer.email=committer@example.invalid"}
	tests := []struct {
		name   string
		config []string
		env    map[string]string
		setup  func(*testing.T)
		want   string
	}{
		{name: "missing both", want: "cannot resolve Git author"},
		{name: "missing name", config: []string{"user.email=fixture@example.invalid"}, want: "cannot resolve Git author"},
		{name: "missing email", config: []string{"user.name=Identity Fixture"}, want: "cannot resolve Git author"},
		{name: "local identity", config: valid},
		{name: "empty name", config: []string{"user.name=", "user.email=fixture@example.invalid"}, want: "cannot resolve Git author"},
		{name: "whitespace name", config: []string{"user.name= \t ", "user.email=fixture@example.invalid"}, want: "cannot resolve Git author"},
		{name: "empty email accepted by git", config: []string{"user.name=Identity Fixture", "user.email="}, want: "empty name or email"},
		{name: "whitespace email", config: []string{"user.name=Identity Fixture", "user.email= \t "}, want: "empty name or email"},
		{name: "role config without user config", config: roles},
		{name: "author alone is insufficient", config: roles[:2], want: "cannot resolve Git committer"},
		{name: "committer alone is insufficient", config: roles[2:], want: "cannot resolve Git author"},
		// Git falls back to user.email for an empty role-specific config value,
		// unlike an explicitly empty GIT_COMMITTER_EMAIL environment override.
		{name: "empty role email falls back to user", config: append(append([]string{}, valid...), "committer.email=")},
		{name: "empty role name falls back to user", config: append(append([]string{}, valid...), "author.name=")},
		{name: "role config overrides empty user config", config: append([]string{"user.name=", "user.email="}, roles...)},
		{name: "empty role email without fallback", config: []string{"author.name=Author Fixture", "author.email=author@example.invalid", "committer.name=Committer Fixture", "committer.email="}, want: "Git committer identity has an empty"},
		{name: "environment without user config", env: map[string]string{"GIT_AUTHOR_NAME": "Author Fixture", "GIT_AUTHOR_EMAIL": "author@example.invalid", "GIT_COMMITTER_NAME": "Committer Fixture", "GIT_COMMITTER_EMAIL": "committer@example.invalid"}},
		{name: "environment overrides empty config", config: []string{"user.name=", "user.email="}, env: map[string]string{"GIT_AUTHOR_NAME": "Author Fixture", "GIT_AUTHOR_EMAIL": "author@example.invalid", "GIT_COMMITTER_NAME": "Committer Fixture", "GIT_COMMITTER_EMAIL": "committer@example.invalid"}},
		{name: "empty author env name", config: valid, env: map[string]string{"GIT_AUTHOR_NAME": ""}, want: "cannot resolve Git author"},
		{name: "empty committer env name", config: valid, env: map[string]string{"GIT_COMMITTER_NAME": ""}, want: "cannot resolve Git committer"},
		{name: "empty author env email", config: valid, env: map[string]string{"GIT_AUTHOR_EMAIL": ""}, want: "Git author identity has an empty"},
		{name: "empty committer env email", config: valid, env: map[string]string{"GIT_COMMITTER_EMAIL": ""}, want: "Git committer identity has an empty"},
		{name: "EMAIL fallback", config: []string{"user.useConfigOnly=false", "user.name=Identity Fixture"}, env: map[string]string{"EMAIL": "fixture@example.invalid"}},
		{name: "EMAIL does not bypass config only", config: []string{"user.name=Identity Fixture"}, env: map[string]string{"EMAIL": "fixture@example.invalid"}, want: "cannot resolve Git author"},
		{name: "injected config", env: map[string]string{"GIT_CONFIG_COUNT": "2", "GIT_CONFIG_KEY_0": "user.name", "GIT_CONFIG_VALUE_0": "Identity Fixture", "GIT_CONFIG_KEY_1": "user.email", "GIT_CONFIG_VALUE_1": "fixture@example.invalid"}},
		{name: "global identity", setup: func(t *testing.T) {
			t.Setenv("GIT_CONFIG_GLOBAL", identityConfigFile(t, "[user]\nname = Global Fixture\nemail = global@example.invalid\n"))
		}},
		{name: "system identity", setup: func(t *testing.T) {
			t.Setenv("GIT_CONFIG_NOSYSTEM", "0")
			t.Setenv("GIT_CONFIG_SYSTEM", identityConfigFile(t, "[user]\nname = System Fixture\nemail = system@example.invalid\n"))
		}},
		{name: "local empty overrides global", config: []string{"user.name="}, setup: func(t *testing.T) {
			t.Setenv("GIT_CONFIG_GLOBAL", identityConfigFile(t, "[user]\nname = Global Fixture\nemail = global@example.invalid\n"))
		}, want: "cannot resolve Git author"},
		{name: "conditional include", setup: func(t *testing.T) {
			path := identityConfigFile(t, "[user]\nname = Included Fixture\nemail = included@example.invalid\n")
			identityGit(t, "symbolic-ref", "HEAD", "refs/heads/identity-fixture")
			identityGit(t, "config", "includeIf.onbranch:identity-fixture.path", filepath.ToSlash(path))
		}},
		{name: "worktree config", setup: func(t *testing.T) {
			identityGit(t, "config", "extensions.worktreeConfig", "true")
			identityGit(t, "config", "--worktree", "user.name", "Worktree Fixture")
			identityGit(t, "config", "--worktree", "user.email", "worktree@example.invalid")
		}},
		{name: "malformed config", setup: func(t *testing.T) {
			t.Setenv("GIT_CONFIG_GLOBAL", identityConfigFile(t, "[broken\n"))
		}, want: "cannot resolve Git author"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupIdentityRepo(t)
			for _, entry := range tt.config {
				key, value, _ := strings.Cut(entry, "=")
				identityGit(t, "config", key, value)
			}
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			if tt.setup != nil {
				tt.setup(t)
			}
			before, err := os.ReadFile(filepath.Join(".git", "config"))
			if err != nil {
				t.Fatal(err)
			}
			err = checkGitCommitIdentity(context.Background())
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				// Prove Git can actually create a commit object with the resolved
				// identity. commit-tree avoids hooks, index changes, and signing.
				tree := identityGit(t, "hash-object", "-w", "-t", "tree", "--stdin")
				identityGit(t, "-c", "commit.gpgsign=false", "commit-tree", tree, "-m", "identity fixture")
			} else if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if err != nil && (strings.Contains(err.Error(), "@example.invalid") || strings.Contains(err.Error(), "Fixture")) {
				t.Fatalf("identity leaked in error: %v", err)
			}
			after, readErr := os.ReadFile(filepath.Join(".git", "config"))
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatalf("identity check changed config: %v", readErr)
			}
		})
	}
}

func TestCheckGitCommitIdentityOutsideRepository(t *testing.T) {
	setupIdentityRepo(t)
	t.Chdir(t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", identityConfigFile(t, "[user]\nuseConfigOnly = true\nname = Global Fixture\nemail = global@example.invalid\n"))
	if err := checkGitCommitIdentity(context.Background()); err != nil {
		t.Fatalf("global identity outside a repository: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", identityConfigFile(t, "[user]\nuseConfigOnly = true\n"))
	if err := checkGitCommitIdentity(context.Background()); err == nil {
		t.Fatal("missing identity outside a repository passed")
	}
}

func TestCheckGitCommitIdentityUnavailable(t *testing.T) {
	setupIdentityRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := checkGitCommitIdentity(ctx); err == nil {
		t.Fatal("cancelled identity check passed")
	}
	t.Setenv("PATH", t.TempDir())
	if err := checkGitCommitIdentity(context.Background()); err == nil {
		t.Fatal("unavailable Git passed")
	}
}

func TestGitIdentityPreflightGuard(t *testing.T) {
	setupIdentityRepo(t)
	env := &axiEnv{repo: &db.Repo{DefaultBranch: "main"}}
	guard := preflightGuard(context.Background(), env, "feature/identity")
	if guard == nil {
		t.Fatal("missing identity passed fresh-run preflight")
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := guard(cmd); err == nil {
		t.Fatal("missing identity did not return an error")
	}
	for _, want := range []string{"Git author identity", "git config user.name", "git config user.email", "user.useConfigOnly"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %s", want, out.String())
		}
	}
	identityGit(t, "config", "user.name", "Identity Fixture")
	identityGit(t, "config", "user.email", "fixture@example.invalid")
	if preflightGuard(context.Background(), env, "feature/identity") != nil {
		t.Fatal("valid identity refused fresh-run preflight")
	}
}

func TestDoctorGitIdentity(t *testing.T) {
	for _, valid := range []bool{false, true} {
		name := "missing"
		if valid {
			name = "valid"
		}
		t.Run(name, func(t *testing.T) {
			setupIdentityRepo(t)
			t.Setenv("NM_HOME", t.TempDir())
			t.Setenv("NO_MISTAKES_TELEMETRY", "off")
			t.Setenv("NO_MISTAKES_NO_UPDATE_CHECK", "1")
			if valid {
				identityGit(t, "config", "user.name", "Identity Fixture")
				identityGit(t, "config", "user.email", "fixture@example.invalid")
			}
			var out bytes.Buffer
			cmd := newDoctorCmd()
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(nil)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("doctor changed its diagnostic exit contract: %v", err)
			}
			want := "cannot resolve Git author identity"
			if valid {
				want = "author and committer have a name and email"
			}
			if !strings.Contains(out.String(), "git identity") || !strings.Contains(out.String(), want) {
				t.Fatalf("missing identity diagnostic %q: %s", want, out.String())
			}
			if !valid && (!strings.Contains(out.String(), gitIdentityHelp) || !strings.Contains(out.String(), "some checks failed")) {
				t.Fatalf("missing repair guidance: %s", out.String())
			}
			if strings.Contains(out.String(), "fixture@example.invalid") || strings.Contains(out.String(), "Identity Fixture") {
				t.Fatalf("doctor printed identity: %s", out.String())
			}
		})
	}
}
