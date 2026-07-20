package repoexec

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidateRuntimeUsesExactSanitizedContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX executable scripts")
	}
	selected := fakeRuntimeContext(t, "account-a", "WRITE", "WRITE")
	t.Setenv("GH_TOKEN", "credential-sentinel")
	t.Setenv("GITHUB_TOKEN", "credential-sentinel")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http.extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_0", "Authorization: credential-sentinel")
	t.Setenv("GIT_ASKPASS", filepath.Join(t.TempDir(), "ambient-askpass"))

	if err := selected.ValidateRuntime(
		context.Background(),
		t.TempDir(),
		"https://github.com/parent/repo.git",
		"https://github.com/account-a/repo.git",
	); err != nil {
		t.Fatalf("validate runtime: %v", err)
	}
}

func TestValidateRuntimeLoginMismatchDoesNotLeakCommandOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX executable scripts")
	}
	selected := fakeRuntimeContext(t, "other-account", "WRITE", "WRITE")
	selected.ExpectedLogin = "expected-account"
	err := selected.ValidateRuntime(context.Background(), t.TempDir(), "https://github.com/parent/repo.git", "")
	if err == nil {
		t.Fatal("expected login mismatch")
	}
	if !strings.Contains(err.Error(), "expected-account") {
		t.Fatalf("error is not actionable: %v", err)
	}
	if strings.Contains(err.Error(), "other-account") || strings.Contains(err.Error(), "credential-sentinel") {
		t.Fatalf("error leaked child output: %v", err)
	}
}

func TestValidateRuntimeFailureSuppressesCredentialSentinel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX executable scripts")
	}
	selected := fakeRuntimeContext(t, "account-a", "WRITE", "WRITE")
	if err := os.WriteFile(selected.GHPath, []byte("#!/bin/sh\nprintf '%s\\n' credential-sentinel >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := selected.ValidateRuntime(context.Background(), t.TempDir(), "https://github.com/parent/repo.git", "")
	if err == nil {
		t.Fatal("expected validation failure")
	}
	if strings.Contains(err.Error(), "credential-sentinel") {
		t.Fatalf("error leaked child output: %v", err)
	}
}

func fakeRuntimeContext(t *testing.T, login, parentPermission, forkPermission string) *GitHubContext {
	t.Helper()
	binDir := t.TempDir()
	configDir := t.TempDir()
	gh := filepath.Join(binDir, "gh")
	git := filepath.Join(binDir, "git")
	ghScript := `#!/bin/sh
if [ "${GH_TOKEN+x}" = x ] || [ "${GITHUB_TOKEN+x}" = x ] || [ "${GIT_ASKPASS+x}" = x ]; then
  printf '%s\n' credential-sentinel >&2
  exit 90
fi
if [ "$GH_CONFIG_DIR" != "` + configDir + `" ] || [ "$GH_HOST" != github.com ]; then
  exit 91
fi
case "$1 $2" in
  "auth status") exit 0 ;;
  "api --hostname") printf '%s\n' '` + login + `' ; exit 0 ;;
  "repo view")
    case "$3" in
      parent/repo) printf '%s\n' '` + parentPermission + `' ;;
      *) printf '%s\n' '` + forkPermission + `' ;;
    esac
    exit 0 ;;
esac
exit 92
`
	gitScript := `#!/bin/sh
if [ "${GH_TOKEN+x}" = x ] || [ "${GIT_ASKPASS+x}" = x ]; then
  printf '%s\n' credential-sentinel >&2
  exit 90
fi
case "$1" in
  --version) printf '%s\n' 'git version fixture' ; exit 0 ;;
  config)
    if [ "$2" = --local ]; then exit 0; fi
    ;;
esac
exit 92
`
	if err := os.WriteFile(gh, []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(git, []byte(gitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return &GitHubContext{
		Version:          GitHubContextVersion,
		GHPath:           gh,
		GitPath:          git,
		GHConfigDir:      configDir,
		Host:             GitHubHost,
		ExpectedLogin:    "account-a",
		GitProtocol:      GitProtocolHTTPS,
		CredentialHelper: CredentialHelperGH,
		CommitAuthor:     CommitAuthor{Name: "Account A", Email: "account-a@example.test"},
		Label:            "account-a",
	}
}
