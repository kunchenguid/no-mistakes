package forgecontext

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// installFakeGH puts a gh on PATH that answers `gh auth token` with
// "token-for-<user>" for the signed-in logins and refuses every other login
// the way gh does. It records its arguments and the GH_CONFIG_DIR it saw, and
// exits 11 when an ambient token variable reaches it, so a test proves the
// stored token was read rather than an inherited one echoed back.
func installFakeGH(t *testing.T, signedIn ...string) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	name := "gh"
	var contents string
	if runtime.GOOS == "windows" {
		name = "gh.cmd"
		var b strings.Builder
		fmt.Fprintf(&b, "@echo off\r\necho %%*^|%%GH_CONFIG_DIR%%> \"%s\"\r\n", argsFile)
		b.WriteString("if defined GH_TOKEN exit /b 11\r\nif defined GITHUB_TOKEN exit /b 11\r\nif defined GH_ENTERPRISE_TOKEN exit /b 11\r\n")
		for _, user := range signedIn {
			fmt.Fprintf(&b, "if \"%%6\"==\"%s\" (echo token-for-%s& exit /b 0)\r\n", user, user)
		}
		b.WriteString("echo no oauth token found for %4 account %6 1>&2\r\nexit /b 1\r\n")
		contents = b.String()
	} else {
		var b strings.Builder
		fmt.Fprintf(&b, "#!/bin/sh\nprintf '%%s|%%s' \"$*\" \"$GH_CONFIG_DIR\" > %q\n", argsFile)
		b.WriteString("[ -z \"$GH_TOKEN$GITHUB_TOKEN$GH_ENTERPRISE_TOKEN\" ] || exit 11\n")
		b.WriteString("case \"$6\" in\n")
		for _, user := range signedIn {
			fmt.Fprintf(&b, "  %s) echo token-for-%s; exit 0;;\n", user, user)
		}
		b.WriteString("esac\necho \"no oauth token found for $4 account $6\" >&2\nexit 1\n")
		contents = b.String()
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_TOKEN", "ambient-active-account")
	t.Setenv("GITHUB_TOKEN", "ambient-active-account")
	t.Setenv("GH_ENTERPRISE_TOKEN", "ambient-active-account")
	return argsFile
}

func readArgs(t *testing.T, argsFile string) (args, configDir string) {
	t.Helper()
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("fake gh was not invoked: %v", err)
	}
	args, configDir, _ = strings.Cut(strings.TrimSpace(string(data)), "|")
	return strings.TrimSpace(args), strings.TrimSpace(configDir)
}

func TestResolveGHUserSelectsTheForkOwnersLoginToken(t *testing.T) {
	argsFile := installFakeGH(t, "remicaesar", "emir-lovie")
	profiles := config.ForgeProfiles{
		"github.com/remicaesar": {GHUser: "remicaesar"},
	}

	resolved, err := Resolve(context.Background(), profiles, "https://github.com/kunchenguid/no-mistakes.git", "https://github.com/RemiCaesar/no-mistakes.git")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved == nil || resolved.Provider != scm.ProviderGitHub || resolved.ProfileHost != "github.com/remicaesar" || resolved.Host != "github.com" {
		t.Fatalf("resolved = %v, want the github.com/remicaesar GitHub profile", resolved)
	}
	args, _ := readArgs(t, argsFile)
	if args != "auth token --hostname github.com --user remicaesar" {
		t.Fatalf("gh args = %q", args)
	}

	env := envMap(resolved.Environment.Apply([]string{
		"GH_TOKEN=ambient",
		"GITHUB_TOKEN=ambient",
		"GH_ENTERPRISE_TOKEN=ambient",
		"GH_HOST=wrong.example.com",
		"GH_REPO=wrong/repo",
		"GH_CONFIG_DIR=/ambient/gh",
		"KEEP=value",
	}))
	if env["GH_TOKEN"] != "token-for-remicaesar" {
		t.Fatalf("GH_TOKEN = %q, want the selected login's token", env["GH_TOKEN"])
	}
	for _, key := range []string{"GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GH_HOST", "GH_REPO"} {
		if _, exists := env[key]; exists {
			t.Fatalf("%s survived the gh_user environment", key)
		}
	}
	if env["GH_CONFIG_DIR"] != "/ambient/gh" || env["KEEP"] != "value" {
		t.Fatalf("ambient gh config or unrelated values changed: %#v", env)
	}
}

// The token reaches subprocesses through the environment only; every way the
// context can be formatted into a log line or error stays free of it.
func TestResolveGHUserNeverFormatsTheToken(t *testing.T) {
	installFakeGH(t, "remicaesar")
	resolved, err := Resolve(context.Background(), config.ForgeProfiles{"github.com/remicaesar": {GHUser: "remicaesar"}}, "git@github.com:remicaesar/repo.git", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, value := range []any{resolved, *resolved, resolved.Environment} {
			if got := fmt.Sprintf(format, value); strings.Contains(got, "token-for-remicaesar") {
				t.Fatalf("%s formatting revealed the token: %s", format, got)
			}
		}
	}
	if got := fmt.Sprint(resolved.Environment); !strings.Contains(got, "GH_TOKEN") {
		t.Fatalf("redacted overlay should still name its variables: %s", got)
	}
}

func TestResolveOwnerScopedKeyOutranksTheParentsHostKey(t *testing.T) {
	installFakeGH(t, "remicaesar", "emir-lovie")
	profiles := config.ForgeProfiles{
		"github.com":            {GHUser: "emir-lovie"},
		"github.com/remicaesar": {GHUser: "remicaesar"},
	}

	resolved, err := Resolve(context.Background(), profiles, "https://github.com/kunchenguid/no-mistakes.git", "https://github.com/remicaesar/no-mistakes.git")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.ProfileHost != "github.com/remicaesar" || envMap(resolved.Environment.Apply([]string{}))["GH_TOKEN"] != "token-for-remicaesar" {
		t.Fatalf("resolved %v, want the owner-scoped profile", resolved)
	}

	plain, err := Resolve(context.Background(), profiles, "https://github.com/kunchenguid/no-mistakes.git", "")
	if err != nil {
		t.Fatalf("Resolve without fork: %v", err)
	}
	if plain.ProfileHost != "github.com" || envMap(plain.Environment.Apply([]string{}))["GH_TOKEN"] != "token-for-emir-lovie" {
		t.Fatalf("resolved %v, want the plain host profile for another owner", plain)
	}
}

func TestResolveDifferentOwnerScopedProfilesForParentAndForkAreAmbiguous(t *testing.T) {
	installFakeGH(t, "a", "b")
	profiles := config.ForgeProfiles{
		"github.com/parent": {GHUser: "a"},
		"github.com/fork":   {GHUser: "b"},
	}
	_, err := Resolve(context.Background(), profiles, "https://github.com/parent/repo.git", "https://github.com/fork/repo.git")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("Resolve error = %v, want ambiguous", err)
	}
}

func TestResolveOwnerScopedKeyFallsBackToThePlainHostKey(t *testing.T) {
	argsFile := installFakeGH(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte("github.com:\n    users:\n        rudingma:\n    user: rudingma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profiles := config.ForgeProfiles{
		"github.com":            {GHConfigDir: dir},
		"github.com/remicaesar": {GHUser: "remicaesar"},
	}

	resolved, err := Resolve(context.Background(), profiles, "https://github.com/rudingma/work-os.git", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.ProfileHost != "github.com" || resolved.ConfigDir != dir {
		t.Fatalf("resolved %v, want the plain host profile", resolved)
	}
	env := envMap(resolved.Environment.Apply([]string{"GH_TOKEN=ambient"}))
	if env["GH_CONFIG_DIR"] != dir {
		t.Fatalf("GH_CONFIG_DIR = %q, want %q", env["GH_CONFIG_DIR"], dir)
	}
	if _, exists := env["GH_TOKEN"]; exists {
		t.Fatal("a gh_config_dir profile must keep removing GH_TOKEN")
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatalf("gh ran for a gh_config_dir profile: %v", err)
	}
}

// An owner-scoped key speaks only for its owner: other owners on the same
// host keep exactly the ambient behavior they had without it.
func TestResolveOwnerScopedKeyLeavesOtherOwnersAmbient(t *testing.T) {
	argsFile := installFakeGH(t, "remicaesar")
	profiles := config.ForgeProfiles{"github.com/remicaesar": {GHUser: "remicaesar"}}

	resolved, err := Resolve(context.Background(), profiles, "https://github.com/kunchenguid/no-mistakes.git", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved != nil {
		t.Fatalf("resolved %v, want ambient (nil) for another owner", resolved)
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Fatalf("gh ran for a repository no profile selects: %v", err)
	}
}

func TestResolveRefusesAGHUserThatIsNotSignedIn(t *testing.T) {
	installFakeGH(t, "emir-lovie")
	profiles := config.ForgeProfiles{"github.com/remicaesar": {GHUser: "remicaesar"}}

	resolved, err := Resolve(context.Background(), profiles, "https://github.com/remicaesar/no-mistakes.git", "")
	if err == nil {
		t.Fatalf("Resolve = %v, want refusal instead of the active account", resolved)
	}
	for _, want := range []string{`forge profile "github.com/remicaesar"`, `gh_user "remicaesar"`, "no oauth token found", "never replaced by the active account"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Resolve error %q missing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "token-for-") || strings.Contains(err.Error(), "ambient-active-account") {
		t.Fatalf("Resolve error revealed a token: %v", err)
	}
}

func TestResolveRefusesWhenGHIsMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Resolve(context.Background(), config.ForgeProfiles{"github.com": {GHUser: "remicaesar"}}, "https://github.com/remicaesar/repo.git", "")
	if err == nil || !strings.Contains(err.Error(), `gh_user "remicaesar"`) {
		t.Fatalf("Resolve error = %v, want gh_user refusal", err)
	}
}

func TestResolveGHUserOnEnterpriseHostUsesTheEnterpriseTokenVariable(t *testing.T) {
	argsFile := installFakeGH(t, "octo")
	resolved, err := Resolve(context.Background(), config.ForgeProfiles{"ghe.example.com/team": {GHUser: "octo"}}, "https://ghe.example.com/team/repo.git", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if args, _ := readArgs(t, argsFile); args != "auth token --hostname ghe.example.com --user octo" {
		t.Fatalf("gh args = %q", args)
	}
	env := envMap(resolved.Environment.Apply([]string{"GH_TOKEN=ambient"}))
	if env["GH_ENTERPRISE_TOKEN"] != "token-for-octo" {
		t.Fatalf("GH_ENTERPRISE_TOKEN = %q, want the selected login's token", env["GH_ENTERPRISE_TOKEN"])
	}
	if _, exists := env["GH_TOKEN"]; exists {
		t.Fatal("GH_TOKEN survived an enterprise gh_user environment")
	}
}

func TestGitHubTokenVariable(t *testing.T) {
	for host, want := range map[string]string{
		"github.com":       "GH_TOKEN",
		"GitHub.com":       "GH_TOKEN",
		"acme.ghe.com":     "GH_TOKEN",
		"ghe.example.com":  "GH_ENTERPRISE_TOKEN",
		"github.acme.corp": "GH_ENTERPRISE_TOKEN",
	} {
		if got := githubTokenVariable(host); got != want {
			t.Fatalf("githubTokenVariable(%q) = %q, want %q", host, got, want)
		}
	}
}
