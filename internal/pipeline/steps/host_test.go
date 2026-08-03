package steps

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// baseFactory records what the wrapper passed down and returns a cmd whose Env
// is nil, matching what stepCmd produces outside tests (nil Env means the child
// inherits the daemon's environment).
func baseFactory(got *[]string) func(context.Context, string, ...string) *exec.Cmd {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*got = append([]string{name}, args...)
		return exec.CommandContext(ctx, "/usr/bin/true", args...)
	}
}

func TestSCMCLIFactoryPassesThroughWhenUnconfigured(t *testing.T) {
	var got []string
	sctx := &pipeline.StepContext{Config: &config.Config{}, Repo: &db.Repo{WorkingPath: "/repo"}}

	scmCLIFactory(sctx, baseFactory(&got))(context.Background(), "gh", "pr", "create")

	if want := []string{"gh", "pr", "create"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("unwrapped invocation = %v, want %v", got, want)
	}
}

func TestSCMCLIFactoryWrapsAndScopesToRepo(t *testing.T) {
	var got []string
	sctx := &pipeline.StepContext{
		Config: &config.Config{SCM: config.SCMRaw{
			CLIWrapper:  []string{"op", "plugin", "run", "--"},
			GHConfigDir: "/gh-config",
		}},
		Repo: &db.Repo{WorkingPath: "/repo"},
	}

	cmd := scmCLIFactory(sctx, baseFactory(&got))(context.Background(), "gh", "pr", "create")

	want := "op plugin run -- gh pr create"
	if strings.Join(got, " ") != want {
		t.Fatalf("wrapped invocation = %q, want %q", strings.Join(got, " "), want)
	}
	// The wrapper resolves directory-scoped credentials from its working
	// directory, so the repo checkout - not the daemon's fixed workdir - has
	// to be the cwd or a work repo would be addressed with a personal token.
	if cmd.Dir != "/repo" {
		t.Fatalf("cmd.Dir = %q, want %q", cmd.Dir, "/repo")
	}
	if !hasEnv(cmd.Env, "GH_CONFIG_DIR=/gh-config") {
		t.Fatalf("GH_CONFIG_DIR missing from env")
	}
	// A nil Env means inherit; overwriting it with just GH_CONFIG_DIR would
	// strip PATH and HOME and break the wrapper itself.
	if len(cmd.Env) < 2 {
		t.Fatalf("env = %v, want the inherited environment plus GH_CONFIG_DIR", cmd.Env)
	}
}

func TestSCMCLIFactoryConfigDirWithoutWrapper(t *testing.T) {
	var got []string
	sctx := &pipeline.StepContext{
		Config: &config.Config{SCM: config.SCMRaw{GHConfigDir: "/gh-config"}},
		Repo:   &db.Repo{WorkingPath: "/repo"},
	}

	cmd := scmCLIFactory(sctx, baseFactory(&got))(context.Background(), "gh", "auth", "status")

	if want := "gh auth status"; strings.Join(got, " ") != want {
		t.Fatalf("invocation = %q, want %q", strings.Join(got, " "), want)
	}
	if !hasEnv(cmd.Env, "GH_CONFIG_DIR=/gh-config") {
		t.Fatalf("GH_CONFIG_DIR missing from env")
	}
	// gh_config_dir travels in the environment. Only the wrapper needs the
	// checkout as its cwd, so setting the config dir alone must not silently
	// move every gh invocation to a different directory.
	if cmd.Dir != "" {
		t.Fatalf("cmd.Dir = %q, want it left as the base factory set it", cmd.Dir)
	}
}

// A step context with no loaded config carries no scm settings at all, so the
// base factory has to come back untouched rather than being wrapped with
// zero-value settings.
func TestSCMCLIFactoryWithoutConfigReturnsBase(t *testing.T) {
	var got []string
	sctx := &pipeline.StepContext{Repo: &db.Repo{WorkingPath: "/repo"}}

	cmd := scmCLIFactory(sctx, baseFactory(&got))(context.Background(), "gh", "pr", "view")

	if want := "gh pr view"; strings.Join(got, " ") != want {
		t.Fatalf("invocation = %q, want %q", strings.Join(got, " "), want)
	}
	if cmd.Dir != "" {
		t.Fatalf("cmd.Dir = %q, want it unchanged without a config", cmd.Dir)
	}
	if cmd.Env != nil {
		t.Fatalf("cmd.Env = %v, want nil (inherit) without a config", cmd.Env)
	}
}

func hasEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}
