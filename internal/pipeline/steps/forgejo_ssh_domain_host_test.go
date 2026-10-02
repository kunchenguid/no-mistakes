package steps

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	forgejoSSHDomainBase   = "https://forge.example:3443/git"
	forgejoSSHDomainRemote = "git@ssh.forge.example:git/octo/widgets.git"
)

// isolateForgejoSSHDomainEnv points glab, gh, and tea configuration discovery at
// empty temp dirs and clears both Forgejo variables, so only a step's own
// environment can answer for the placeholder hosts below.
func isolateForgejoSSHDomainEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GLAB_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FORGEJO_BASE_URL", "")
	t.Setenv("FORGEJO_SSH_DOMAIN", "")
}

func TestDetectProviderForStep_UsesForgejoSSHDomainFromStepEnvironment(t *testing.T) {
	isolateForgejoSSHDomainEnv(t)
	sctx := &pipeline.StepContext{
		Ctx: context.Background(),
		Env: []string{
			"FORGEJO_BASE_URL=" + forgejoSSHDomainBase,
			"FORGEJO_SSH_DOMAIN=ssh.forge.example",
		},
	}
	if got := detectProviderForStep(sctx, forgejoSSHDomainRemote); got != scm.ProviderForgejo {
		t.Fatalf("detectProviderForStep() = %q, want %q", got, scm.ProviderForgejo)
	}

	// The base URL alone cannot recognize an origin published on another host.
	sctx.Env = []string{"FORGEJO_BASE_URL=" + forgejoSSHDomainBase}
	if got := detectProviderForStep(sctx, forgejoSSHDomainRemote); got != scm.ProviderUnknown {
		t.Fatalf("detectProviderForStep() without an SSH domain = %q, want %q", got, scm.ProviderUnknown)
	}
}

func TestBuildHost_ForgejoAcceptsSSHDomainFromStepEnvironment(t *testing.T) {
	isolateForgejoSSHDomainEnv(t)
	sctx := &pipeline.StepContext{
		Ctx: context.Background(),
		Run: &db.Run{Branch: "feature/forgejo", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Repo: &db.Repo{
			UpstreamURL:   forgejoSSHDomainRemote,
			DefaultBranch: "main",
		},
		Config: &config.Config{ForgejoAXIPath: "/opt/tools/forgejo-axi"},
		Env: []string{
			"FORGEJO_BASE_URL=" + forgejoSSHDomainBase,
			"FORGEJO_SSH_DOMAIN=ssh.forge.example",
		},
	}

	host, reason := buildHost(sctx, scm.ProviderForgejo)
	if host == nil || reason != "" {
		t.Fatalf("buildHost() = (%v, %q), want Forgejo host", host, reason)
	}
	if host.Provider() != scm.ProviderForgejo {
		t.Fatalf("Provider() = %q, want %q", host.Provider(), scm.ProviderForgejo)
	}

	// Without the declared domain the same remote cannot be pinned to the
	// configured instance, so host construction fails loudly instead of guessing.
	sctx.Env = []string{"FORGEJO_BASE_URL=" + forgejoSSHDomainBase}
	host, reason = buildHost(sctx, scm.ProviderForgejo)
	if host != nil || !strings.Contains(reason, "does not match") {
		t.Fatalf("buildHost() = (%v, %q), want an explicit host mismatch", host, reason)
	}
}
