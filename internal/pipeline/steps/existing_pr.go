package steps

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
)

func existingPRURL(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.Run == nil || sctx.Run.ExistingPRURL == nil {
		return ""
	}
	return *sctx.Run.ExistingPRURL
}

// ValidateExistingPR is shared by launch, pre-push and publication. An explicit
// target is a constraint, not a suggestion: unavailable validation never skips.
// The returned host is the validated one, so callers never rebuild it and can
// never reach it without the validation that proved the target.
func ValidateExistingPR(sctx *pipeline.StepContext, head string) (scm.Host, *scm.PR, error) {
	host, pr, err := ValidateExistingPRIdentity(sctx)
	if err != nil || pr == nil {
		return host, pr, err
	}
	if head == "" {
		return nil, nil, staleAssociation(sctx, fmt.Errorf("the push remote has no head for the source branch, so there is nothing to validate the pull request against"))
	}
	if pr.HeadSHA != head {
		return nil, nil, fmt.Errorf("%w: %s is at %s, not the %s this run published: its source branch moved outside this run - validate the new head with a fresh run on the branch, or run `no-mistakes axi run --retire-existing-pr` on it if that pull request is no longer the target", errExplicitPRHeadMismatch, existingPRURL(sctx), shortSHA(pr.HeadSHA), shortSHA(head))
	}
	return host, pr, nil
}

// errExplicitPRHeadMismatch marks the head check specifically, so a caller that
// has just proven that head on the source remote can tell it apart from every
// other association failure.
var errExplicitPRHeadMismatch = errors.New("explicit PR head mismatch")

// A pull request is served from a replica, so its head can still answer the
// commit before the one this run just verified on the source remote. These
// bound the re-read: a few seconds, then the mismatch stands.
var (
	prHeadSettleAttempts = 3
	prHeadSettleDelay    = 2 * time.Second
)

// ValidateExistingPublishedPR is ValidateExistingPR for a head this run already
// verified on the source remote. Equality stays exact - an ancestor is never
// accepted and the refusal is unchanged once the bound is spent - but the
// forge's own read lag is re-read rather than reported as a source branch that
// moved outside the run.
func ValidateExistingPublishedPR(sctx *pipeline.StepContext, head string) (scm.Host, *scm.PR, error) {
	host, pr, err := ValidateExistingPR(sctx, head)
	for attempt := 1; attempt < prHeadSettleAttempts && errors.Is(err, errExplicitPRHeadMismatch); attempt++ {
		select {
		case <-sctx.Ctx.Done():
			return nil, nil, sctx.Ctx.Err()
		case <-time.After(prHeadSettleDelay):
		}
		host, pr, err = ValidateExistingPR(sctx, head)
	}
	return host, pr, err
}

// ValidateExistingPRIdentity proves the association alone: the target is an
// open pull request in its repository whose source is this run's push
// repository and branch. A run that is about to publish a new head has nothing
// to compare the live head against yet, so only the identity is required.
func ValidateExistingPRIdentity(sctx *pipeline.StepContext) (scm.Host, *scm.PR, error) {
	raw := existingPRURL(sctx)
	if raw == "" {
		return nil, nil, nil
	}
	if sctx.Run.PRURL == nil || *sctx.Run.PRURL != raw {
		return nil, nil, staleAssociation(sctx, fmt.Errorf("explicit PR differs from persisted publication identity"))
	}
	host, reason := buildHost(sctx, resolvedProvider(sctx))
	if host == nil {
		return nil, nil, fmt.Errorf("validate explicit PR: %s", reason)
	}
	gh, ok := host.(*github.Host)
	if !ok {
		return nil, nil, fmt.Errorf("explicit PR is supported only for GitHub")
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return nil, nil, fmt.Errorf("validate explicit PR: %w", err)
	}
	pushURL := resolvePushURL(sctx)
	if scm.ResolveHost(sctx.Ctx, pushURL) != "github.com" {
		return nil, nil, fmt.Errorf("explicit PR source must be a github.com push repository")
	}
	pr, err := gh.ValidateExistingPRIdentity(sctx.Ctx, raw, github.RepoSlug(pushURL), sctx.Run.Branch)
	if err != nil {
		return nil, nil, staleAssociation(sctx, err)
	}
	return host, pr, nil
}

// staleAssociation carries the supported way out of an association that no
// longer describes this branch - a merged or closed pull request, a source ref
// that moved - so the operator is not left repeating a failing run. It never
// retires anything itself and never falls back to another pull request.
func staleAssociation(sctx *pipeline.StepContext, err error) error {
	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	return fmt.Errorf("%w; %s publishes to %s - if that is no longer the right target, run `no-mistakes axi run --retire-existing-pr` on the branch to return to ordinary discovery", err, branch, existingPRURL(sctx))
}

// explicitTargetRepo is the owner/repo the run's pull request lives in, or ""
// for a run with no association.
func explicitTargetRepo(sctx *pipeline.StepContext) string {
	target := existingPRURL(sctx)
	if target == "" {
		return ""
	}
	repo, _, err := github.ExistingPRTarget(target)
	if err != nil {
		return ""
	}
	return repo
}

// integrationBranchPromptLabel names the branch a prompt's base commit was
// measured from. An associated run measures against a branch in the pull
// request's repository, which is not the fork "origin" points at; every other
// run keeps naming the repository default exactly as before.
func integrationBranchPromptLabel(sctx *pipeline.StepContext) string {
	if explicitTargetRepo(sctx) != "" {
		return integrationBranchLabel(sctx, runIntegrationBranch(sctx))
	}
	return sctx.Repo.DefaultBranch
}

// integrationBranchLabel names the branch a run integrates with the way an
// operator reads it: an associated run integrates with a branch in the pull
// request's repository, which is not the fork "origin" points at.
func integrationBranchLabel(sctx *pipeline.StepContext, branch string) string {
	if repo := explicitTargetRepo(sctx); repo != "" {
		return repo + ":" + branch
	}
	return "origin/" + branch
}
