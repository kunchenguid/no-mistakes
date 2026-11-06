package daemon

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

// Validate availability and trusted instruction suppression before cancellation
// or row creation. An unavailable explicit reviewer must never become a fallback.
func (m *RunManager) validateReviewerBeforeCancel(ctx context.Context, repo *db.Repo, head string, global *config.GlobalConfig, pin *agentcfg.ReviewerProfile) error {
	cfg, _, err := m.effectiveConfigBeforeRun(ctx, repo, head, global, "reviewer selection")
	if err != nil {
		return err
	}
	return m.validateReviewerAvailability(ctx, cfg, pin)
}

func (m *RunManager) validateReviewerAvailability(ctx context.Context, cfg *config.Config, pin *agentcfg.ReviewerProfile) error {
	if err := cfg.ApplyReviewerProfile(pin); err != nil {
		return err
	}
	role := cfg.ForReviewAgent(cfg.ReviewAgents[config.RoleReviewer])
	role.QuotaRouting.Enabled = false
	ag, _, err := newConfiguredAgent(ctx, role, m.paths.EvidenceRoot(cfg.Test.Evidence.LocalRoot), exec.LookPath, runenv.Overlay{}, nil)
	if err != nil {
		return fmt.Errorf("requested reviewer unavailable: %w", err)
	}
	return ag.Close()
}

func (m *RunManager) resolveReviewerProfile(ctx context.Context, repoID string, request *agentcfg.ReviewerProfile) (*agentcfg.ReviewerProfile, error) {
	if request == nil {
		return nil, fmt.Errorf("reviewer selection is required")
	}
	global, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		return nil, err
	}
	pin, err := global.ResolveReviewerProfile(request)
	if err != nil {
		return nil, err
	}
	repo, err := m.db.GetRepo(repoID)
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return nil, fmt.Errorf("unknown reviewer repository")
	}
	trustedSHA, err := fetchTrustedDefaultBranchSHA(ctx, m.paths.RepoDir(repo.ID), repo, "reviewer selection")
	if err != nil {
		return nil, err
	}
	trusted, err := effectiveRepoConfigAt(ctx, m.paths.RepoDir(repo.ID), repo, trustedSHA, trustedSHA, "reviewer selection")
	if err != nil {
		return nil, err
	}
	cfg := config.MergeForRemote(global, trusted, repo.UpstreamURL)
	if err := m.validateReviewerAvailability(ctx, cfg, pin); err != nil {
		return nil, err
	}
	return pin, nil
}
