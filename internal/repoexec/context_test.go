package repoexec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func validTestContext(t *testing.T) *GitHubContext {
	t.Helper()
	binDir := t.TempDir()
	gh := filepath.Join(binDir, executableName("gh"))
	git := filepath.Join(binDir, executableName("git"))
	for _, path := range []string{gh, git} {
		if err := os.WriteFile(path, []byte("test executable"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	configDir := t.TempDir()
	return &GitHubContext{
		Version:          1,
		GHPath:           gh,
		GitPath:          git,
		GHConfigDir:      configDir,
		Host:             "github.com",
		ExpectedLogin:    "account-a",
		GitProtocol:      "https",
		CredentialHelper: "gh",
		CommitAuthor: CommitAuthor{
			Name:  "Account A",
			Email: "account-a@example.test",
		},
		Label: "account-a",
	}
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func TestLoadGitHubContextRejectsSecretAndUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.json")
	contents := `{
  "version": 1,
  "gh_path": "/usr/bin/gh",
  "git_path": "/usr/bin/git",
  "gh_config_dir": "/tmp/gh-a",
  "host": "github.com",
  "expected_login": "account-a",
  "git_protocol": "https",
  "credential_helper": "gh",
  "commit_author": {"name": "A", "email": "a@example.test"},
  "token": "credential-sentinel"
}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadGitHubContext(path)
	if err == nil {
		t.Fatal("expected unknown credential field to be rejected")
	}
	if strings.Contains(err.Error(), "credential-sentinel") {
		t.Fatalf("error leaked credential value: %v", err)
	}
}

func TestGitHubContextValidateStaticRequiresHTTPSGitHubDotCom(t *testing.T) {
	ctx := validTestContext(t)
	for _, tc := range []struct {
		name     string
		upstream string
		fork     string
	}{
		{name: "ssh upstream", upstream: "git@github.com:owner/repo.git"},
		{name: "enterprise", upstream: "https://github.example.com/owner/repo.git"},
		{name: "userinfo", upstream: "https://credential-sentinel@github.com/owner/repo.git"},
		{name: "ssh fork", upstream: "https://github.com/owner/repo.git", fork: "git@github.com:fork/repo.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ctx.ValidateStatic(tc.upstream, tc.fork)
			if err == nil {
				t.Fatal("expected strict URL validation failure")
			}
			if strings.Contains(err.Error(), "credential-sentinel") {
				t.Fatalf("error leaked URL userinfo: %v", err)
			}
		})
	}
	if err := ctx.ValidateStatic("https://github.com/owner/repo.git", "https://github.com/fork/repo.git"); err != nil {
		t.Fatalf("valid strict context: %v", err)
	}
}

func TestGitHubContextEnvironmentRemovesAmbientCredentialOverrides(t *testing.T) {
	ctx := validTestContext(t)
	base := []string{
		"PATH=" + filepath.Join(t.TempDir(), "ambient-wrapper"),
		"GH_TOKEN=credential-sentinel",
		"GITHUB_TOKEN=credential-sentinel",
		"GH_CONFIG_DIR=/ambient/gh",
		"GH_HOST=elsewhere.example",
		"GH_REPO=other/repo",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: credential-sentinel",
		"GIT_CONFIG_PARAMETERS='credential.helper=ambient'",
		"GIT_CONFIG_GLOBAL=/ambient/gitconfig",
		"GIT_CONFIG_SYSTEM=/ambient/system-gitconfig",
		"GIT_ASKPASS=/ambient/askpass",
		"SSH_ASKPASS=/ambient/ssh-askpass",
		"GIT_SSH_COMMAND=ssh -i /ambient/key",
		"SSH_AUTH_SOCK=/ambient/agent",
		"GIT_TRACE_CURL=/tmp/credential-sentinel-trace",
		"GIT_EXEC_PATH=/ambient/git-core",
		"GIT_DIR=/ambient/repository.git",
		"GIT_AUTHOR_NAME=Ambient Author",
		"GIT_AUTHOR_EMAIL=ambient@example.test",
	}

	env := ctx.Environment(base, "/work/repo")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "credential-sentinel") || strings.Contains(joined, "/ambient/askpass") || strings.Contains(joined, "Ambient Author") {
		t.Fatalf("strict environment retained ambient credential or identity override:\n%s", joined)
	}
	for _, want := range []string{
		"GH_CONFIG_DIR=" + ctx.GHConfigDir,
		"GH_HOST=github.com",
		"GH_PROMPT_DISABLED=1",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	} {
		if !containsEnv(env, want) {
			t.Errorf("strict environment missing %q", want)
		}
	}
	path := envValue(env, "PATH")
	if !strings.HasPrefix(path, filepath.Dir(ctx.GHPath)+string(os.PathListSeparator)) {
		t.Fatalf("PATH = %q, want selected executable directory first", path)
	}

	gitConfig := gitConfigFromEnv(t, env)
	for key, want := range map[string][]string{
		"credential.helper":  {"", credentialHelperCommand(ctx.GHPath)},
		"user.name":          {ctx.CommitAuthor.Name},
		"user.email":         {ctx.CommitAuthor.Email},
		"user.useConfigOnly": {"true"},
		"http.extraHeader":   {""},
	} {
		got := gitConfig[key]
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("git config %s = %#v, want %#v", key, got, want)
		}
	}
}

func TestGitHubContextIsScopedByContextForConcurrentRepositories(t *testing.T) {
	a := validTestContext(t)
	b := validTestContext(t)
	b.ExpectedLogin = "account-b"
	b.Label = "account-b"
	b.CommitAuthor = CommitAuthor{Name: "Account B", Email: "account-b@example.test"}

	type result struct {
		ctx *GitHubContext
		env []string
	}
	results := make(chan result, 2)
	for _, selected := range []*GitHubContext{a, b} {
		selected := selected
		go func() {
			ctx := WithGitHubContext(context.Background(), selected)
			got, ok := GitHubContextFrom(ctx)
			if !ok {
				results <- result{}
				return
			}
			results <- result{ctx: got, env: got.Environment([]string{"GH_TOKEN=credential-sentinel"}, "/work")}
		}()
	}

	seen := map[string]bool{}
	for range 2 {
		got := <-results
		if got.ctx == nil {
			t.Fatal("missing context in concurrent worker")
		}
		seen[got.ctx.ExpectedLogin] = true
		if strings.Contains(strings.Join(got.env, "\n"), "credential-sentinel") {
			t.Fatal("ambient token crossed into selected context")
		}
		config := gitConfigFromEnv(t, got.env)
		if names := config["user.name"]; len(names) != 1 || names[0] != got.ctx.CommitAuthor.Name {
			t.Fatalf("identity crossed contexts: %#v for %s", names, got.ctx.ExpectedLogin)
		}
	}
	if !seen["account-a"] || !seen["account-b"] {
		t.Fatalf("concurrent contexts = %#v", seen)
	}
}

func containsEnv(env []string, entry string) bool {
	for _, candidate := range env {
		if candidate == entry {
			return true
		}
	}
	return false
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func gitConfigFromEnv(t *testing.T, env []string) map[string][]string {
	t.Helper()
	countText := envValue(env, "GIT_CONFIG_COUNT")
	var count int
	if _, err := fmt.Sscanf(countText, "%d", &count); err != nil {
		t.Fatalf("parse GIT_CONFIG_COUNT %q: %v", countText, err)
	}
	got := make(map[string][]string)
	for i := 0; i < count; i++ {
		key := envValue(env, fmt.Sprintf("GIT_CONFIG_KEY_%d", i))
		value := envValue(env, fmt.Sprintf("GIT_CONFIG_VALUE_%d", i))
		got[key] = append(got[key], value)
	}
	return got
}
