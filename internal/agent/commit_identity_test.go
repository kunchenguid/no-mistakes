package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

func identityAgentRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "absent-global"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	identityAgentGit(t, dir, "init")
	identityAgentGit(t, dir, "config", "commit.gpgsign", "false")
	return dir
}

func identityAgentGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitSafeEnv(dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestCommitIdentityAgentEnvironmentPreservesRepositoryIdentity(t *testing.T) {
	dir := identityAgentRepo(t)
	identityAgentGit(t, dir, "config", "extensions.worktreeConfig", "true")
	identity := git.CommitIdentity{AuthorName: "Submitting Author", AuthorEmail: "author@example.test", CommitterName: "Submitting Committer", CommitterEmail: "committer@example.test"}
	if err := git.CopyLocalUserIdentity(context.Background(), dir, identity); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "fixture")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	identityAgentGit(t, nested, "init")
	identityAgentGit(t, nested, "config", "user.name", "Fixture")
	identityAgentGit(t, nested, "config", "user.email", "fixture@example.test")
	identityAgentGit(t, nested, "config", "commit.gpgsign", "false")
	for _, role := range []string{"AUTHOR", "COMMITTER"} {
		t.Setenv("GIT_"+role+"_NAME", "Daemon")
		t.Setenv("GIT_"+role+"_EMAIL", "daemon@example.test")
		t.Setenv("GIT_"+role+"_DATE", "2000-01-01T00:00:00Z")
	}
	overlay := runenv.Overlay{Set: map[string]string{
		"GIT_AUTHOR_NAME": "Forge Override", "GIT_CONFIG_KEY_99": "author.name", "GIT_CONFIG_VALUE_99": "Orphan Forge",
	}}
	t.Setenv("GIT_CONFIG_COUNT", "3")
	t.Setenv("GIT_CONFIG_KEY_0", "author.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "Daemon Config Author")
	t.Setenv("GIT_CONFIG_KEY_1", "author.email")
	t.Setenv("GIT_CONFIG_VALUE_1", "daemon-config@example.test")
	t.Setenv("GIT_CONFIG_KEY_2", "test.retained")
	t.Setenv("GIT_CONFIG_VALUE_2", "count value")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'committer.name'='Daemon Config Committer' 'committer.email=daemon-config@example.test' 'test.parameter'='O'\\''Brien = value'")
	for target, want := range map[string]string{
		dir:    "Submitting Author <author@example.test> | Submitting Committer <committer@example.test>",
		nested: "Fixture <fixture@example.test> | Fixture <fixture@example.test>",
	} {
		cmd := exec.Command("git", "commit", "--allow-empty", "-m", "agent subprocess")
		cmd.Dir = target
		cmd.Env = gitSafeEnvWithOverlay(target, overlay, []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=author.name", "GIT_CONFIG_VALUE_0=Adapter Override", "GIT_COMMITTER_EMAIL=adapter@example.test"})
		resolved := resolveAgentEnv(cmd.Env)
		for _, key := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1", "GIT_CONFIG_KEY_2", "GIT_CONFIG_VALUE_2", "GIT_CONFIG_KEY_99", "GIT_CONFIG_VALUE_99"} {
			if _, exists := resolved[key]; exists {
				t.Errorf("agent environment retained %s", key)
			}
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("agent commit: %v: %s", err, out)
		}
		if got := identityAgentGit(t, target, "show", "-s", "--format=%an <%ae> | %cn <%ce>"); got != want {
			t.Errorf("%s identity = %q, want %q", target, got, want)
		}
		if got := identityAgentGit(t, target, "show", "-s", "--format=%at %ct"); got == "946684800 946684800" {
			t.Error("agent inherited daemon commit dates")
		}
		for _, key := range []string{"test.retained", "test.parameter"} {
			read := exec.Command("git", "config", "--get", key)
			read.Dir, read.Env = target, cmd.Env
			out, err := read.CombinedOutput()
			if err == nil || len(out) != 0 {
				t.Errorf("inherited command-scope %s survived: %q, %v", key, out, err)
			}
		}
	}
}

func TestCommitIdentityInheritedIncludeCannotOverrideRun(t *testing.T) {
	dir := identityAgentRepo(t)
	identity := git.CommitIdentity{AuthorName: "Submitter", AuthorEmail: "submitter@example.test", CommitterName: "Submitter", CommitterEmail: "submitter@example.test"}
	if err := git.CopyLocalUserIdentity(context.Background(), dir, identity); err != nil {
		t.Fatal(err)
	}
	included := filepath.Join(t.TempDir(), "daemon-config")
	if err := os.WriteFile(included, []byte("[author]\nname = Included Daemon\nemail = included@example.test\n[committer]\nname = Included Daemon\nemail = included@example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "include.path")
	t.Setenv("GIT_CONFIG_VALUE_0", included)
	cmd := exec.Command("git", "commit", "--allow-empty", "-m", "inherited include")
	cmd.Dir, cmd.Env = dir, gitSafeEnv(dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("agent commit: %v: %s", err, out)
	}
	want := "Submitter <submitter@example.test> | Submitter <submitter@example.test>"
	if got := identityAgentGit(t, dir, "show", "-s", "--format=%an <%ae> | %cn <%ce>"); got != want {
		t.Fatalf("command-scope include replaced run identity: %q, want %q", got, want)
	}
}
