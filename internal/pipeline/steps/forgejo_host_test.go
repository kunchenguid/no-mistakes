package steps

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestDetectProviderForStep_UsesForgejoBaseFromStepEnvironment(t *testing.T) {
	sctx := &pipeline.StepContext{Env: []string{"FORGEJO_BASE_URL=https://code.example:3443/scm"}}
	got := detectProviderForStep(sctx, "https://code.example:3443/scm/octo/widgets.git")
	if got != scm.ProviderForgejo {
		t.Fatalf("detectProviderForStep() = %q, want %q", got, scm.ProviderForgejo)
	}
}

func TestForgejoTokenEnvForStep_PrefersHostScopedToken(t *testing.T) {
	sctx := &pipeline.StepContext{Env: []string{
		"FORGEJO_TOKEN=generic-secret",
		"FORGEJO_TOKEN_FORGE_2E_EXAMPLE_3A_8443=host-secret",
	}}
	got := forgejoTokenEnvForStep(sctx, "https://forge.example:8443/git")
	if got != "FORGEJO_TOKEN_FORGE_2E_EXAMPLE_3A_8443" {
		t.Fatalf("forgejoTokenEnvForStep() = %q, want host-scoped token name", got)
	}

	sctx.Env = []string{"FORGEJO_TOKEN=generic-secret"}
	if got := forgejoTokenEnvForStep(sctx, "https://forge.example:8443/git"); got != "FORGEJO_TOKEN" {
		t.Fatalf("forgejoTokenEnvForStep() = %q, want generic fallback", got)
	}
}

func TestBuildHost_Forgejo(t *testing.T) {
	sctx := &pipeline.StepContext{
		Ctx: context.Background(),
		Run: &db.Run{Branch: "feature/forgejo", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Repo: &db.Repo{
			UpstreamURL:   "https://forge.example:3443/git/octo/widgets.git",
			DefaultBranch: "main",
		},
		Config: &config.Config{ForgejoAXIPath: "/opt/tools/forgejo-axi"},
		Env:    []string{"FORGEJO_BASE_URL="},
	}

	host, reason := buildHost(sctx, scm.ProviderForgejo)
	if host == nil || reason != "" {
		t.Fatalf("buildHost() = (%v, %q), want Forgejo host", host, reason)
	}
	if host.Provider() != scm.ProviderForgejo {
		t.Fatalf("Provider() = %q, want %q", host.Provider(), scm.ProviderForgejo)
	}
}

func TestVerifyMergedProof_RequiresProofForExpectedHead(t *testing.T) {
	pr := &scm.PR{Number: "42", URL: "https://forge.example/octo/widgets/pulls/42"}
	host := &mergedProofTestHost{proof: scm.MergedProof{
		Merged:  true,
		Number:  "42",
		URL:     pr.URL,
		HeadSHA: "unexpected",
	}}

	err := verifyMergedProof(context.Background(), host, pr, "expected")
	if !errors.Is(err, scm.ErrHeadChanged) {
		t.Fatalf("verifyMergedProof() error = %v, want ErrHeadChanged", err)
	}
}

func TestVerifyMergedProof_RejectsIncompleteProof(t *testing.T) {
	pr := &scm.PR{Number: "42", URL: "https://forge.example/octo/widgets/pulls/42"}
	host := &mergedProofTestHost{proof: scm.MergedProof{Number: "42", URL: pr.URL, HeadSHA: "expected"}}
	if err := verifyMergedProof(context.Background(), host, pr, "expected"); err == nil {
		t.Fatal("verifyMergedProof() error = nil, want unmerged proof rejection")
	}
}

func TestBuildHost_ForgejoRejectsForkRouting(t *testing.T) {
	sctx := &pipeline.StepContext{
		Ctx:    context.Background(),
		Run:    &db.Run{},
		Repo:   &db.Repo{UpstreamURL: "https://codeberg.org/octo/widgets.git", ForkURL: "https://codeberg.org/alice/widgets.git"},
		Config: &config.Config{ForgejoAXIPath: "forgejo-axi"},
	}

	host, reason := buildHost(sctx, scm.ProviderForgejo)
	if host != nil || !strings.Contains(reason, "fork") {
		t.Fatalf("buildHost() = (%v, %q), want explicit fork rejection", host, reason)
	}
}

type mergedProofTestHost struct {
	scm.Host
	proof scm.MergedProof
	err   error
}

func (h *mergedProofTestHost) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergedProof: true}
}

func (h *mergedProofTestHost) GetMergedProof(_ context.Context, _ *scm.PR, expectedHead string) (scm.MergedProof, error) {
	if h.err != nil {
		return scm.MergedProof{}, h.err
	}
	if h.proof.HeadSHA != expectedHead {
		return scm.MergedProof{}, scm.ErrHeadChanged
	}
	return h.proof, nil
}
