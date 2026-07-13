package github

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCreateSecretGistFallsBackToGitForBinaryFiles exercises the carried-forward
// git-based binary gist fallback (fm/nm-gist-evidence-g2) end to end: when
// `gh gist create` rejects a binary evidence file with "binary file not
// supported", CreateSecretGist must seed a gist, clone it, stage the real binary
// bytes, commit, and push. This drives a real local git remote (a bare repo
// standing in for the gist) and asserts the binary landed byte-for-byte.
func TestCreateSecretGistFallsBackToGitForBinaryFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	tmp := t.TempDir()
	const gistID = "abc123deadbeef"

	// Bare repo stands in for the remote gist. https://gist.github.com/<id>.git
	// is redirected here via url.insteadOf in a private GIT_CONFIG_GLOBAL.
	bareRepo := filepath.Join(tmp, "remote", gistID+".git")
	if err := os.MkdirAll(filepath.Dir(bareRepo), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "init", "--bare", "--initial-branch=main", bareRepo)

	gitConfig := filepath.Join(tmp, "gitconfig")
	// The remote path must be forward-slashed in the file:// URL: git config
	// treats a backslash as an escape char (Windows paths would be mangled), and
	// a Windows drive path needs the file:///C:/... triple-slash form so the
	// drive letter is not parsed as a URL authority.
	remoteURL := filepath.ToSlash(filepath.Join(tmp, "remote"))
	if !strings.HasPrefix(remoteURL, "/") {
		remoteURL = "/" + remoteURL
	}
	cfg := fmt.Sprintf(`[url "file://%s/"]
	insteadOf = https://gist.github.com/
[protocol "file"]
	allow = always
[safe]
	directory = *
[init]
	defaultBranch = main
`, remoteURL)
	if err := os.WriteFile(gitConfig, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	// A genuine binary evidence file with NUL bytes, like a PNG screenshot.
	binaryPath := filepath.Join(tmp, "screenshot.png")
	binaryBytes := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00binary-evidence\x00\xff\xfe")
	if err := os.WriteFile(binaryPath, binaryBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	gistURL := "https://gist.github.com/user/" + gistID
	apiJSON := fmt.Sprintf(
		`{"files":{"screenshot.png":{"filename":"screenshot.png","raw_url":"https://gist.githubusercontent.com/user/%s/raw/screenshot.png"}}}`,
		gistID)

	cleanEnv := gitEnv(gitConfig)

	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "git" {
			c := exec.CommandContext(ctx, name, args...)
			c.Env = cleanEnv
			return c
		}
		if name != "gh" {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		// gh gist create with the binary path -> reject; with the seed -> succeed.
		if len(args) >= 2 && args[0] == "gist" && args[1] == "create" {
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "no-mistakes-gist-") { // seed README create
				return fakeGh(ctx, gistURL+"\n", "", 0)
			}
			return fakeGh(ctx, "", "gist create failed: binary file not supported\n", 1)
		}
		if len(args) >= 2 && args[0] == "api" && args[1] == "gists/"+gistID {
			return fakeGh(ctx, apiJSON, "", 0)
		}
		t.Fatalf("unexpected gh invocation: %v", args)
		return nil
	}

	host := New(factory, func() bool { return true }, "github.com", "test/repo")

	gist, err := host.CreateSecretGist(context.Background(), []string{binaryPath})
	if err != nil {
		t.Fatalf("CreateSecretGist() error = %v", err)
	}
	if gist.ID != gistID {
		t.Fatalf("gist ID = %q, want %q", gist.ID, gistID)
	}
	if gist.URL != gistURL {
		t.Fatalf("gist URL = %q, want %q", gist.URL, gistURL)
	}
	if len(gist.Files) != 1 || gist.Files[0].Filename != "screenshot.png" {
		t.Fatalf("gist files = %+v, want single screenshot.png", gist.Files)
	}

	// Prove the binary bytes were actually pushed to the remote gist.
	verifyDir := filepath.Join(tmp, "verify")
	runGitEnv(t, tmp, cleanEnv, "clone", "https://gist.github.com/"+gistID+".git", verifyDir)
	got, err := os.ReadFile(filepath.Join(verifyDir, "screenshot.png"))
	if err != nil {
		t.Fatalf("read pushed binary: %v", err)
	}
	if string(got) != string(binaryBytes) {
		t.Fatalf("pushed binary bytes = %q, want %q", got, binaryBytes)
	}
}

func fakeGh(ctx context.Context, stdout, stderr string, code int) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestGitHubHelperProcess", "--", "fake-gh")
	cmd.Env = append(os.Environ(),
		"GITHUB_TEST_HELPER=1",
		"GITHUB_TEST_STDOUT="+stdout,
		"GITHUB_TEST_STDERR="+stderr,
		fmt.Sprintf("GITHUB_TEST_EXIT_CODE=%d", code),
	)
	return cmd
}

// gitEnv strips ambient GIT_CONFIG_* injection (agent harnesses leak it) and
// pins a private global config carrying the gist URL redirect.
func gitEnv(gitConfig string) []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_CONFIG_") || strings.HasPrefix(kv, "GIT_CONFIG=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"GIT_CONFIG_GLOBAL="+gitConfig,
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	return env
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	runGitEnv(t, dir, nil, args...)
}

func runGitEnv(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
