package steps

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/kunchenguid/no-mistakes/internal/bitbucket"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitlab"
)

// buildHost returns a scm.Host for the given provider, wired to sctx's
// working directory and environment. When the host cannot be constructed
// (unknown provider, missing Bitbucket config, etc) it returns nil and a
// human-readable skip reason suitable for logging.
func buildHost(sctx *pipeline.StepContext, provider scm.Provider) (scm.Host, string) {
	cmdFactory := func(_ context.Context, name string, args ...string) *exec.Cmd {
		return stepCmd(sctx, name, args...)
	}
	switch provider {
	case scm.ProviderGitHub:
		return github.New(cmdFactory, func() bool { return stepCLIAvailable(sctx, provider) }), ""
	case scm.ProviderGitLab:
		return gitlab.New(cmdFactory, func() bool { return stepCLIAvailable(sctx, provider) }), ""
	case scm.ProviderBitbucket:
		client, err := bitbucket.NewClientFromEnv(sctx.Env)
		if err != nil {
			return nil, err.Error()
		}
		repo, err := resolveBitbucketRepoRef(sctx.Repo.UpstreamURL, sctx.Run.PRURL)
		if err != nil {
			return nil, err.Error()
		}
		return bitbucket.NewHost(client, repo), ""
	default:
		return nil, fmt.Sprintf("provider %s is not supported yet", provider)
	}
}
