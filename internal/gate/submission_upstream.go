package gate

import (
	"context"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/integration"
)

func SubmissionUpstream(ctx context.Context, workDir, defaultBranch, head, requested string, remoteTimeout time.Duration) (upstream ReconciliationUpstream) {
	defer func() { upstream.RemoteTimeout = remoteTimeout }()
	remoteCtx := func() (context.Context, context.CancelFunc) {
		if remoteTimeout > 0 {
			return context.WithTimeout(ctx, remoteTimeout)
		}
		return ctx, func() {}
	}
	var upstreamURL string
	var err error
	base := strings.TrimSpace(defaultBranch)
	if integration.RequestedRemote(ctx) != "" {
		destination, resolveErr := integration.ResolveRequest(ctx, workDir)
		if resolveErr != nil {
			return ReconciliationUpstream{}
		}
		upstreamURL, err = destination.RuntimeURL(ctx, workDir)
		base = destination.DefaultBranch
	} else {
		upstreamURL, err = git.GetRemoteURL(ctx, workDir, "origin")
	}
	if err != nil || strings.TrimSpace(upstreamURL) == "" {
		return ReconciliationUpstream{}
	}
	if requested = strings.TrimSpace(requested); requested != "" {
		return ReconciliationUpstream{URL: upstreamURL, BaseBranch: requested}
	}
	if base == "" {
		return ReconciliationUpstream{}
	}
	ref := "refs/heads/" + base
	lsCtx, lsCancel := remoteCtx()
	sha, err := git.LsRemote(lsCtx, workDir, upstreamURL, ref)
	lsCancel()
	if err != nil || sha == "" {
		return ReconciliationUpstream{}
	}
	fetchCtx, fetchCancel := remoteCtx()
	err = git.FetchRemoteRef(fetchCtx, workDir, upstreamURL, ref, sha)
	fetchCancel()
	if err != nil {
		return ReconciliationUpstream{}
	}
	readConfig := func(commit string) (*config.RepoConfig, error) {
		entry, err := git.Run(ctx, workDir, "ls-tree", "--full-tree", commit, "--", ".no-mistakes.yaml")
		if err != nil {
			return nil, err
		}
		if entry == "" {
			return &config.RepoConfig{}, nil
		}
		content, err := git.ShowFile(ctx, workDir, commit, ".no-mistakes.yaml")
		if err != nil {
			return nil, err
		}
		return config.LoadRepoFromBytes([]byte(content))
	}
	trusted, err := readConfig(sha)
	if err != nil {
		return ReconciliationUpstream{}
	}
	pushed, err := readConfig(head)
	if err != nil {
		return ReconciliationUpstream{}
	}
	effective := config.EffectiveRepoConfig(pushed, trusted, trusted.AllowRepoCommands)
	if selected := strings.TrimSpace(effective.PR.BaseBranch); selected != "" {
		return ReconciliationUpstream{URL: upstreamURL, BaseBranch: selected}
	}
	return ReconciliationUpstream{URL: upstreamURL, BaseBranch: base}
}
