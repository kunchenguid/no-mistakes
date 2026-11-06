package steps

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/integration"
)

func TestIntegrationRemotePRDestinationAndForkHead(t *testing.T) {
	for _, provider := range []string{"github", "gitlab", "github-fork"} {
		t.Run(provider, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			remote := "https://github.com/customer/project.git"
			if provider == "gitlab" {
				remote = "https://gitlab.com/customer/project.git"
			}
			gitCmd(t, dir, "remote", "add", "customer", remote)
			gitCmd(t, dir, "config", "url."+dir+".insteadOf", remote)
			gitCmd(t, dir, "branch", "release/customer", base)
			destination, err := integration.ResolveRequest(integration.WithRemote(context.Background(), "customer"), dir)
			if err != nil {
				t.Fatal(err)
			}
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(`{"title":"fix: customer release","body":"## What Changed\n\nFix the release."}`)}, nil
			}}
			sctx := newPRTestContext(t, ag, dir, base, head, config.Commands{})
			sctx.Run.Integration = destination
			sctx.Repo.Integration = destination
			sctx.Repo.UpstreamURL = destination.URL
			sctx.Repo.DefaultBranch = destination.DefaultBranch
			branch := "release/customer"
			sctx.Run.PRBaseBranch = &branch
			var logFile string
			if provider == "gitlab" {
				sctx.Env, logFile, _ = fakeGlabWithMRState(t)
				sctx.Env = append(sctx.Env, "FAKE_CLI_LOG_REPO=1")
			} else {
				sctx.Env, logFile = fakeGH(t, "")
			}
			if provider == "github-fork" {
				sctx.Repo.ForkURL = "https://github.com/contributor/project.git"
				sctx.Env = append(sctx.Env, "FAKE_CLI_PR_SOURCE_REPO=contributor/project")
			}
			recordPRTestPush(t, sctx)
			if _, err := (&PRStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(logFile)
			if err != nil {
				t.Fatal(err)
			}
			log := string(data)
			project := "--repo customer/project"
			if provider == "gitlab" {
				project = "GITLAB_REPO=https://gitlab.com/customer/project.git"
			}
			for _, want := range []string{project, "release/customer"} {
				if !strings.Contains(log, want) {
					t.Fatalf("missing %q in forge calls:\n%s", want, log)
				}
			}
			if provider == "github-fork" && !strings.Contains(log, "--head contributor:feature") {
				t.Fatalf("fork head was confused with integration target:\n%s", log)
			}
		})
	}
}

func TestIntegrationRemoteMovedSelectionRefusesBeforePublication(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	gitCmd(t, dir, "remote", "add", "customer", dir)
	destination, err := integration.ResolveRequest(integration.WithRemote(context.Background(), "customer"), dir)
	if err != nil {
		t.Fatal(err)
	}
	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Run.Integration = destination
	sctx.Repo.Integration = destination
	sctx.Repo.UpstreamURL = destination.URL
	sctx.Repo.ForkURL = "https://github.com/contributor/project.git"
	sctx.Env, _ = fakeGH(t, "")
	gitCmd(t, dir, "remote", "set-url", "customer", t.TempDir())
	if err := fetchRunUpstreamBranch(context.Background(), sctx, "main"); err == nil {
		t.Fatal("moved destination fetched through a fallback")
	}
	if err := publishRunHead(sctx, head, "", nil); err == nil {
		t.Fatal("a fork push bypassed the moved integration destination refusal")
	}
	if _, err := (&PRStep{}).Execute(sctx); err == nil {
		t.Fatal("moved destination reached PR publication")
	}
}
