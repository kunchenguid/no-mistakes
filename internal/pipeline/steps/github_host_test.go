package steps

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestBuildHostGitHubCanonicalizesSSHTransportEndpoint(t *testing.T) {
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "ssh")
	linkTestBinary(t, binDir, "gh")

	const (
		remote  = "git@github.com:kunchenguid/no-mistakes.git"
		forkURL = "https://github.com/HackXIt/no-mistakes.git"
		prURL   = "https://github.com/kunchenguid/no-mistakes/pull/1016"
	)
	vars := map[string]string{
		"FAKE_CLI_MODE": "github-ssh-transport-endpoint",
		"FAKE_CLI_PR_LIST_JSON": `[{"number":1016,"url":"https://github.com/kunchenguid/no-mistakes/pull/1016",` +
			`"baseRefName":"main","headRefName":"feature/github-ssh-transport","headRepositoryOwner":{"login":"HackXIt"}}]`,
	}
	path := binDir + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", path)
	for key, value := range vars {
		t.Setenv(key, value)
	}

	profileDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(profileDir, "hosts.yml"), []byte("github.com:\n    user: contributor\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	forgeCtx, err := forgecontext.Resolve(context.Background(), config.ForgeProfiles{
		"github.com": {GHConfigDir: profileDir},
	}, remote, forkURL)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if forgeCtx == nil || forgeCtx.Host != "github.com" {
		t.Fatalf("Resolve() = %#v, want canonical GitHub forge host", forgeCtx)
	}

	sctx := &pipeline.StepContext{
		Ctx: context.Background(),
		Run: &db.Run{Branch: "feature/github-ssh-transport"},
		Repo: &db.Repo{
			UpstreamURL:   remote,
			ForkURL:       forkURL,
			DefaultBranch: "main",
		},
		Env:          fakeCLIEnv(binDir, vars),
		ForgeContext: forgeCtx,
	}
	host, reason := buildHost(sctx, scm.ProviderGitHub)
	if host == nil || reason != "" {
		t.Fatalf("buildHost() = (%v, %q), want GitHub host", host, reason)
	}
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}

	pr, err := host.FindPR(context.Background(), "feature/github-ssh-transport", "main")
	if err != nil {
		t.Fatalf("FindPR() error = %v", err)
	}
	if pr == nil || pr.URL != prURL {
		t.Fatalf("FindPR() = %+v, want PR at %s", pr, prURL)
	}
}
