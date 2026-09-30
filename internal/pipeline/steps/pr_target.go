package steps

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/azuredevops"
)

// ResolvePRTarget selects an exact, read-only forge observation before the
// pipeline uses a target branch. It does not attach or update a PR.
func ResolvePRTarget(sctx *pipeline.StepContext) (pipeline.PRTargetSelection, error) {
	if selected, ok := localOnlyPRTarget(sctx); ok {
		return selected, nil
	}
	host, reason := buildHost(sctx, resolvedProvider(sctx))
	if host == nil {
		return pipeline.PRTargetSelection{}, fmt.Errorf("read pull request target: %s", reason)
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return pipeline.PRTargetSelection{}, fmt.Errorf("read pull request target: %w", err)
	}
	reader, ok := host.(scm.PRFactsReader)
	if !ok {
		return pipeline.PRTargetSelection{}, fmt.Errorf("provider cannot read complete pull request facts")
	}
	return resolvePRTargetWithReader(sctx, reader, false)
}

// A local Git remote has no forge PR to discover. It still needs a pinned
// comparison against the configured prospective target for Review and Test.
// Unknown network forges and recorded PRs must keep failing closed.
func localOnlyPRTarget(sctx *pipeline.StepContext) (pipeline.PRTargetSelection, bool) {
	if sctx == nil || sctx.Run == nil || sctx.Repo == nil ||
		resolvedProvider(sctx) != scm.ProviderUnknown || runPRURL(sctx) != "" {
		return pipeline.PRTargetSelection{}, false
	}
	remote := strings.TrimSpace(sctx.Repo.UpstreamURL)
	if !strings.HasPrefix(remote, "file://") && !filepath.IsAbs(remote) {
		return pipeline.PRTargetSelection{}, false
	}
	return pipeline.PRTargetSelection{
		SourceBranch: strings.TrimPrefix(sctx.Run.Branch, "refs/heads/"),
		TargetBranch: effectivePRBaseBranch(sctx),
	}, true
}

func resolvePRTargetWithReader(sctx *pipeline.StepContext, reader scm.PRFactsReader, allowTerminal bool) (pipeline.PRTargetSelection, error) {
	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	pushURL := strings.TrimSpace(sctx.Repo.PushURL())
	sourceRepo := scm.RepoPath(pushURL)
	if resolvedProvider(sctx) == scm.ProviderAzureDevOps {
		canonical, err := azuredevops.CanonicalSourceRepository(pushURL)
		if err != nil {
			return pipeline.PRTargetSelection{}, fmt.Errorf("canonicalize Azure PR source repository: %w", err)
		}
		sourceRepo = canonical
	}
	if sourceRepo == "" || branch == "" {
		return pipeline.PRTargetSelection{}, fmt.Errorf("cannot resolve pull request source repository or branch")
	}
	selected := pipeline.PRTargetSelection{SourceRepo: sourceRepo, SourceBranch: branch}
	owned := runPRURL(sctx)
	if owned != "" {
		facts, err := reader.ReadPRFacts(sctx.Ctx, prFromOwnedURL(owned))
		if err != nil {
			return pipeline.PRTargetSelection{}, fmt.Errorf("read recorded pull request %s: %w", owned, err)
		}
		if err := validatePRFacts(facts, resolvedProvider(sctx), sourceRepo, branch, allowTerminal); err != nil {
			return pipeline.PRTargetSelection{}, fmt.Errorf("recorded pull request %s: %w", owned, err)
		}
		if !samePRIdentity(owned, &facts.PR) {
			return pipeline.PRTargetSelection{}, fmt.Errorf("recorded pull request %s has a different forge identity", owned)
		}
		selected.PRURL = facts.PR.URL
		selected.ForgeHeadSHA = facts.HeadSHA
		selected.TargetBranch = facts.BaseBranch
		selected.State = facts.State
		return selected, nil
	}

	candidates, err := reader.FindOpenPRFacts(sctx.Ctx, sourceRepo, branch)
	if err != nil {
		return pipeline.PRTargetSelection{}, fmt.Errorf("find open pull requests for %s on %s: %w", sourceRepo, branch, err)
	}
	if len(candidates) > 1 {
		return pipeline.PRTargetSelection{}, fmt.Errorf("multiple open pull requests for %s on %s", sourceRepo, branch)
	}
	if len(candidates) == 0 {
		selected.TargetBranch = effectivePRBaseBranch(sctx)
		return selected, nil
	}
	facts := candidates[0]
	if err := validatePRFacts(facts, resolvedProvider(sctx), sourceRepo, branch, false); err != nil {
		return pipeline.PRTargetSelection{}, fmt.Errorf("discovered pull request: %w", err)
	}
	localHead, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return pipeline.PRTargetSelection{}, fmt.Errorf("read local head before selecting pull request target: %w", err)
	}
	selected.ForgeHeadSHA = facts.HeadSHA
	selected.State = facts.State
	if facts.HeadSHA != localHead {
		// A fresh run may contain local commits or a rebase that have not
		// reached the existing PR yet. Its live base is still the comparison
		// target, but the PR is not attached until Push updates its head and
		// a later read proves the exact local revision is published.
		selected.TargetBranch = facts.BaseBranch
		return selected, nil
	}
	selected.PRURL = facts.PR.URL
	selected.TargetBranch = facts.BaseBranch
	return selected, nil
}

func validatePRFacts(facts scm.PRFacts, provider scm.Provider, sourceRepo, branch string, allowTerminal bool) error {
	if facts.State != scm.PRStateOpen && !(allowTerminal && (facts.State == scm.PRStateMerged || facts.State == scm.PRStateClosed)) {
		return fmt.Errorf("pull request %s is %s", facts.PR.URL, strings.ToLower(string(facts.State)))
	}
	if !scm.SameSourceRepository(provider, facts.SourceRepository, sourceRepo) || facts.SourceBranch != branch {
		return fmt.Errorf("pull request %s source %s on %s differs from %s on %s", facts.PR.URL, facts.SourceRepository, facts.SourceBranch, sourceRepo, branch)
	}
	if strings.TrimSpace(facts.PR.URL) == "" || strings.TrimSpace(facts.HeadSHA) == "" || strings.TrimSpace(facts.BaseBranch) == "" {
		return fmt.Errorf("pull request facts lack URL, head, or target branch")
	}
	return nil
}

// currentPRTargetBranch returns the forge target for a run already bound to a
// pull request. A run without a PR uses the same prospective target as PR
// creation. A recorded PR must be readable: stale config cannot replace its
// live base when a diff or integration decision is made.
func currentPRTargetBranch(sctx *pipeline.StepContext) (string, error) {
	if sctx.PRTarget != nil {
		if sctx.PRTarget.TargetBranch == "" {
			return "", fmt.Errorf("selected pull request target branch is empty")
		}
		return sctx.PRTarget.TargetBranch, nil
	}
	prospective := effectivePRBaseBranch(sctx)
	owned := runPRURL(sctx)
	if owned == "" {
		return prospective, nil
	}
	host, reason := buildHost(sctx, resolvedProvider(sctx))
	if host == nil {
		return "", fmt.Errorf("read pull request %s target: %s", owned, reason)
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return "", fmt.Errorf("read pull request %s target: %w", owned, err)
	}
	state, err := host.GetPRState(sctx.Ctx, prFromOwnedURL(owned))
	if err != nil {
		return "", fmt.Errorf("read pull request %s state: %w", owned, err)
	}
	if state != scm.PRStateOpen {
		return "", fmt.Errorf("recorded pull request %s is %s", owned, strings.ToLower(string(state)))
	}
	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	discovered, err := host.FindPR(sctx.Ctx, branch, "")
	if err != nil {
		return "", fmt.Errorf("find pull request for %s: %w", branch, err)
	}
	if discovered == nil || !samePRIdentity(owned, discovered) {
		return "", fmt.Errorf("recorded pull request %s does not match the open pull request on %s", owned, branch)
	}
	pr := discovered
	base := strings.TrimSpace(pr.BaseBranch)
	if base == "" {
		reader, ok := host.(scm.PRBaseBranchReader)
		if !ok {
			return "", fmt.Errorf("provider cannot read pull request %s target", owned)
		}
		base, err = reader.GetPRBaseBranch(sctx.Ctx, pr)
		if err != nil {
			return "", fmt.Errorf("read pull request %s target: %w", owned, err)
		}
		base = strings.TrimSpace(base)
	}
	if base == "" {
		return "", fmt.Errorf("pull request %s has no readable target branch", owned)
	}
	return base, nil
}
