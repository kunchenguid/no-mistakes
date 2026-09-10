package steps

import (
	"context"
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestBuildHostGitLabResolvesSSHConfigAliasForPRDiscovery(t *testing.T) {
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "ssh")
	linkTestBinary(t, binDir, "glab")

	const (
		alias    = "gitlab-work"
		hostname = "gitlab.compass.tools"
		remote   = "git@gitlab-work:qa/test-automation.git"
		mrURL    = "https://gitlab.compass.tools/qa/test-automation/-/merge_requests/170"
		mrJSON   = `{"iid":170,"web_url":"https://gitlab.compass.tools/qa/test-automation/-/merge_requests/170"}`
		repoJSON = `{"web_url":"https://gitlab.compass.tools/qa/test-automation","path_with_namespace":"qa/test-automation"}`
	)
	vars := map[string]string{
		"FAKE_CLI_MODE":           "gitlab-ssh-config-alias",
		"FAKE_CLI_SSH_ALIAS":      alias,
		"FAKE_CLI_SSH_HOSTNAME":   hostname,
		"FAKE_CLI_MR_VIEW_JSON":   mrJSON,
		"FAKE_CLI_REPO_VIEW_JSON": repoJSON,
	}
	path := binDir + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", path)
	for key, value := range vars {
		t.Setenv(key, value)
	}

	sctx := &pipeline.StepContext{
		Ctx:  context.Background(),
		Run:  &db.Run{Branch: "feature/ssh-alias"},
		Repo: &db.Repo{UpstreamURL: remote, DefaultBranch: "main"},
		Env:  fakeCLIEnv(binDir, vars),
	}
	host, reason := buildHost(sctx, scm.ProviderGitLab)
	if host == nil || reason != "" {
		t.Fatalf("buildHost() = (%v, %q), want GitLab host", host, reason)
	}
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}

	pr, err := host.FindPR(context.Background(), "feature/ssh-alias", "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil || pr.Number != "170" || pr.URL != mrURL {
		t.Fatalf("FindPR() = %+v, want MR 170 at %s", pr, mrURL)
	}
}
