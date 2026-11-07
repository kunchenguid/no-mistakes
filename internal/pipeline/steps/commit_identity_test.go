package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCorrectionCommitIdentityAcrossProducers(t *testing.T) {
	for _, step := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepCI} {
		t.Run(string(step), func(t *testing.T) {
			sctx, dir, _ := agentCommitContext(t, nil)
			identity := git.CommitIdentity{AuthorName: "Submitter", AuthorEmail: "submitter@example.test", CommitterName: "Submitter", CommitterEmail: "submitter@example.test"}
			sctx.Ctx = git.WithCommitIdentity(context.Background(), identity)
			t.Setenv("GIT_AUTHOR_NAME", "Daemon")
			t.Setenv("GIT_AUTHOR_EMAIL", "daemon@example.test")
			t.Setenv("GIT_COMMITTER_NAME", "Daemon")
			t.Setenv("GIT_COMMITTER_EMAIL", "daemon@example.test")
			writeAgentChange(t, dir)
			if step == types.StepCI {
				sctx.Config.CI.RevalidateRepairs = true
				if _, err := (&CIStep{}).commitRepair(sctx, "repair", nil); err != nil {
					t.Fatal(err)
				}
			} else if _, err := commitAgentFixesWithResult(sctx, step, "repair", "fallback", nil); err != nil {
				t.Fatal(err)
			}
			if got := gitCmd(t, dir, "show", "-s", "--format=%an <%ae> | %cn <%ce>"); got != "Submitter <submitter@example.test> | Submitter <submitter@example.test>" {
				t.Fatalf("%s correction identity = %q", step, got)
			}
		})
	}
}

func TestAgentCommitRewritePreservesAuthorAndBindsCommitter(t *testing.T) {
	sctx, dir, _ := agentCommitContext(t, nil)
	identity := git.CommitIdentity{AuthorName: "Submitter", AuthorEmail: "submitter@example.test", CommitterName: "Submitter", CommitterEmail: "submitter@example.test"}
	sctx.Ctx = git.WithCommitIdentity(context.Background(), identity)
	if err := os.WriteFile(filepath.Join(dir, "agent-change"), []byte("repair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The fake agent runs the same subprocess path as step commands. An explicit
	// per-commit author is intentional attribution and survives message shaping.
	if _, err := stepGitRun(sctx, "add", "agent-change"); err != nil {
		t.Fatal(err)
	}
	if _, err := stepGitRun(sctx, "commit", "--author=Contributor <contributor@example.test>", "-m", "agent's message"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_COMMITTER_NAME", "Daemon")
	t.Setenv("GIT_COMMITTER_EMAIL", "daemon@example.test")
	if _, err := commitAgentFixesWithResult(sctx, types.StepReview, "repair", "fallback", nil); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, dir, "show", "-s", "--format=%an <%ae> | %cn <%ce>"); got != "Contributor <contributor@example.test> | Submitter <submitter@example.test>" {
		t.Fatalf("rewritten commit identity = %q", got)
	}
}

func TestCommitIdentitySubprocessesPreserveRepositoryIdentity(t *testing.T) {
	for _, entry := range []string{"shell", "repository command", "configured test"} {
		t.Run(entry, func(t *testing.T) {
			sctx, dir, _ := agentCommitContext(t, nil)
			identity := git.CommitIdentity{AuthorName: "Submitting Author", AuthorEmail: "author@example.test", CommitterName: "Submitting Committer", CommitterEmail: "committer@example.test"}
			if err := git.CopyLocalUserIdentity(context.Background(), dir, identity); err != nil {
				t.Fatal(err)
			}
			sctx.Ctx = git.WithCommitIdentity(context.Background(), identity)
			nested := filepath.Join(dir, "fixture")
			if err := os.Mkdir(nested, 0o755); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, nested, "init")
			gitCmd(t, nested, "config", "user.name", "Fixture")
			gitCmd(t, nested, "config", "user.email", "fixture@example.test")
			gitCmd(t, nested, "config", "commit.gpgsign", "false")
			for _, role := range []string{"AUTHOR", "COMMITTER"} {
				t.Setenv("GIT_"+role+"_NAME", "Daemon")
				t.Setenv("GIT_"+role+"_EMAIL", "daemon@example.test")
				t.Setenv("GIT_"+role+"_DATE", "2000-01-01T00:00:00Z")
				sctx.Env = append(sctx.Env, "GIT_"+role+"_NAME=Step Override", "GIT_"+role+"_EMAIL=step@example.test")
			}
			t.Setenv("GIT_CONFIG_COUNT", "1")
			t.Setenv("GIT_CONFIG_KEY_0", "include.path")
			t.Setenv("GIT_CONFIG_VALUE_0", filepath.Join(dir, "daemon-config"))
			if err := os.WriteFile(filepath.Join(dir, "daemon-config"), []byte("[author]\nname = Included Daemon\nemail = included@example.test\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			sctx.ForgeContext = &forgecontext.Context{Environment: runenv.Overlay{Set: map[string]string{
				"GIT_AUTHOR_NAME": "Forge Override", "GIT_CONFIG_KEY_99": "author.name", "GIT_CONFIG_VALUE_99": "Orphan Forge",
			}}}
			for _, target := range []string{".", "fixture"} {
				sctx.Env = append(sctx.Env,
					"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=author.name", "GIT_CONFIG_VALUE_0=Step Config Author",
					"GIT_CONFIG_KEY_1=author.email", "GIT_CONFIG_VALUE_1=step-config@example.test",
					"GIT_CONFIG_PARAMETERS='committer.name=Step Config Committer' 'committer.email'='step-config@example.test'",
				)
				command := "git -C " + target + " commit --allow-empty -m subprocess"
				var out string
				var code int
				var err error
				if entry == "shell" {
					out, code, err = runStepShellCommand(sctx, command)
				} else if entry == "configured test" {
					out, code, err = runRepositoryCommand(sctx, "test", command)
				} else {
					out, code, err = executeRepositoryCommand(sctx, "identity", command)
				}
				if err != nil || code != 0 {
					t.Fatalf("%s: exit=%d err=%v: %s", entry, code, err, out)
				}
			}
			for target, want := range map[string]string{
				dir:    "Submitting Author <author@example.test> | Submitting Committer <committer@example.test>",
				nested: "Fixture <fixture@example.test> | Fixture <fixture@example.test>",
			} {
				if got := gitCmd(t, target, "show", "-s", "--format=%an <%ae> | %cn <%ce>"); got != want {
					t.Errorf("%s identity = %q, want %q", target, got, want)
				}
				if got := gitCmd(t, target, "show", "-s", "--format=%at %ct"); got == "946684800 946684800" {
					t.Error("subprocess inherited daemon commit dates")
				}
			}
		})
	}
}

func TestCommandScopeGitConfigIsLimitedToPipelineOperations(t *testing.T) {
	sctx, _, head := agentCommitContext(t, nil)
	identity := git.CommitIdentity{AuthorName: "Submitter", AuthorEmail: "submitter@example.test", CommitterName: "Submitter", CommitterEmail: "submitter@example.test"}
	sctx.Ctx = git.WithCommitIdentity(sctx.Ctx, identity)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http.extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_0", "X-Synthetic: operator")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'http.proxy=http://synthetic.invalid:3128'")
	sctx.Env, _ = fakeGH(t, "https://example.test/owner/repo/pull/1")
	sctx.Env = append(sctx.Env, "FAKE_CLI_MODE=git-passthrough")
	sctx.ForgeContext = &forgecontext.Context{Environment: runenv.Overlay{Set: map[string]string{
		"GIT_CONFIG_KEY_99": "http.extraHeader", "GIT_CONFIG_VALUE_99": "Orphan Forge",
	}}}
	for _, entry := range []string{"git", "git tree", "git env", "provider"} {
		t.Run(entry, func(t *testing.T) {
			for key, want := range map[string]string{
				"http.extraHeader": "X-Synthetic: operator",
				"http.proxy":       "http://synthetic.invalid:3128",
			} {
				var got string
				var err error
				switch entry {
				case "git":
					got, err = stepGitRun(sctx, "config", "--get", key)
				case "git tree":
					got, err = stepGitRunTree(sctx, "config", "--get", key)
				case "git env":
					got, err = stepGitRunEnv(sctx, []string{"GIT_AUTHOR_NAME=Contributor"}, "config", "--get", key)
				case "provider":
					out, runErr := stepCmd(sctx, "gh", "config", "--get", key).CombinedOutput()
					got, err = strings.TrimSpace(string(out)), runErr
				}
				if err != nil || got != want {
					t.Fatalf("%s config %s = %q, %v, want %q", entry, key, got, err, want)
				}
			}
		})
	}
	for _, entry := range []string{"configured test", "shell", "base test"} {
		t.Run(entry, func(t *testing.T) {
			for _, key := range []string{"http.extraHeader", "http.proxy"} {
				command := "git config --get " + key
				var out string
				var code int
				var err error
				switch entry {
				case "configured test":
					out, code, err = runRepositoryCommand(sctx, "test", command)
				case "shell":
					out, code, err = runStepShellCommand(sctx, command)
				case "base test":
					out, code, err = runTestCommandOnBase(sctx, command, head)
				}
				if err != nil || code != 1 || strings.TrimSpace(out) != "" {
					t.Fatalf("%s inherited %s: %q, exit=%d, err=%v", entry, key, out, code, err)
				}
			}
		})
	}
}
