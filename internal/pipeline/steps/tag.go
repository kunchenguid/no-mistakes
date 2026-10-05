package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// runTagRef reports whether the run publishes a tag rather than a branch.
// Tags never get a PR: their delivery evidence is the pushed tag's identity
// and the checks reported for the commit it peels to.
func runTagRef(sctx *pipeline.StepContext) (string, bool) {
	if sctx == nil || sctx.Run == nil {
		return "", false
	}
	return sctx.Run.Branch, strings.HasPrefix(sctx.Run.Branch, "refs/tags/")
}

// verifyPublishedTag peels both the run head and the tag on the push target
// to commits and returns that commit only when they agree. Annotated tags
// advertise the peeled commit as "<ref>^{}"; lightweight tags point at it.
func verifyPublishedTag(sctx *pipeline.StepContext, ref string) (string, error) {
	want, err := stepGitRun(sctx, "rev-parse", "--verify", sctx.Run.HeadSHA+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("peel run head %s: %w", sctx.Run.HeadSHA, err)
	}
	out, err := stepGitRun(sctx, "ls-remote", resolvePushURL(sctx), ref, ref+"^{}")
	if err != nil {
		return "", fmt.Errorf("read %s on the push target: %w", ref, err)
	}
	got := ""
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[1] == ref+"^{}" || (fields[1] == ref && got == "") {
			got = fields[0]
		}
	}
	if got == "" {
		return "", fmt.Errorf("%s is not present on the push target", ref)
	}
	if got != strings.TrimSpace(want) {
		return "", fmt.Errorf("%s on the push target peels to %s, want reviewed commit %s", ref, got, strings.TrimSpace(want))
	}
	return got, nil
}

// resolveTagPush returns the local tag object to publish for ref and how to
// publish it. Pushing the tag object rather than its commit keeps an annotated
// tag annotated. Tags are immutable: an existing remote or gate mirror tag must
// already be that exact object, and a different one is refused before anything
// is published, never force-moved.
func resolveTagPush(ctx context.Context, gitRun gitRunner, gateDir, pushURL, ref, head string) (string, forcePushDecision, error) {
	object, err := gitRun("rev-parse", "--verify", ref)
	if err != nil {
		return "", forcePushDecision{}, fmt.Errorf("resolve local %s: %w", ref, err)
	}
	object = strings.TrimSpace(object)
	peeled, err := gitRun("rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", forcePushDecision{}, fmt.Errorf("peel local %s: %w", ref, err)
	}
	if strings.TrimSpace(peeled) != head {
		return "", forcePushDecision{}, fmt.Errorf("local %s peels to %s, not the reviewed head %s", ref, strings.TrimSpace(peeled), head)
	}
	if gateDir = strings.TrimSpace(gateDir); gateDir != "" {
		if _, statErr := os.Stat(gateDir); statErr == nil {
			gateTag, _, err := git.DirectRefTarget(ctx, gateDir, ref)
			if err != nil {
				return "", forcePushDecision{}, fmt.Errorf("inspect gate mirror %s: %w", ref, err)
			}
			if gateTag != "" && gateTag != object {
				return "", forcePushDecision{}, fmt.Errorf("gate mirror tag %s is %s, not the reviewed tag object %s; tags are never moved", ref, gateTag, object)
			}
		} else if !os.IsNotExist(statErr) {
			return "", forcePushDecision{}, fmt.Errorf("stat gate mirror repository: %w", statErr)
		}
	}
	current, err := lsRemoteSHA(gitRun, pushURL, ref)
	if err != nil {
		return "", forcePushDecision{}, fmt.Errorf("resolve remote %s: %w", ref, err)
	}
	switch current {
	case "":
		return object, forcePushDecision{newBranch: true}, nil
	case object:
		return object, forcePushDecision{remoteSHA: current, upToDate: true}, nil
	}
	return "", forcePushDecision{}, fmt.Errorf("%s already exists on the push target as %s, not the reviewed tag object %s; tags are never moved", ref, current, object)
}

// tagHost builds the provider host for the repository the tag was pushed to.
// Branch PRs target the upstream, but a tag pushed to a fork only ran the
// fork's workflows.
func tagHost(sctx *pipeline.StepContext) (scm.Host, string) {
	if sctx.Repo == nil || strings.TrimSpace(sctx.Repo.ForkURL) == "" {
		return buildHost(sctx, resolvedProvider(sctx))
	}
	repo := *sctx.Repo
	repo.UpstreamURL, repo.ForkURL = repo.ForkURL, ""
	forkCtx := *sctx
	forkCtx.Repo = &repo
	return buildHost(&forkCtx, resolvedProvider(&forkCtx))
}

// monitorTagChecks waits for the checks the tag push triggered on its commit.
// A tag is delivered only when at least one check reported and all of them
// settled without failing. Failures, providers without tag checks, and the
// timeout (absolute from the first poll) park for the user instead of passing.
func (s *CIStep) monitorTagChecks(sctx *pipeline.StepContext, host scm.Host, ref string) (*pipeline.StepOutcome, error) {
	sha, err := verifyPublishedTag(sctx, ref)
	if err != nil {
		return nil, err
	}
	reader, ok := host.(scm.TagChecksHost)
	if !ok {
		return ciFailureOutcome(nil, false, fmt.Sprintf("%s cannot read tag push checks, so CI for %s is unverified", host.Provider(), ref)), nil
	}
	tag := strings.TrimPrefix(ref, "refs/tags/")
	timeout := sctx.Config.CITimeout
	unlimited := timeout < 0
	if timeout == 0 {
		timeout = config.DefaultCITimeout
	}
	now := s.now
	if now == nil {
		now = time.Now
	}
	started := now()
	sctx.Log(fmt.Sprintf("monitoring CI for tag %s at %s...", ref, sha))
	for {
		if err := sctx.Ctx.Err(); err != nil {
			return nil, err
		}
		checks, err := reader.GetTagChecks(sctx.Ctx, tag, sha)
		if errors.Is(err, scm.ErrTagProvenance) {
			return ciFailureOutcome(nil, false, fmt.Sprintf("CI for tag %s is unverified: %v", ref, err)), nil
		}
		if err != nil {
			return nil, err
		}
		var failing []scm.CheckTarget
		pending := len(checks) == 0
		for _, c := range checks {
			switch c.Bucket {
			case scm.CheckBucketFail, scm.CheckBucketCancel:
				failing = append(failing, scm.CheckTarget{Name: c.Name, ProviderID: c.ProviderID})
			case scm.CheckBucketPass, scm.CheckBucketSkip:
			default:
				pending = true
			}
		}
		if len(failing) > 0 {
			return ciFailureOutcome(failing, false, fmt.Sprintf("CI failed for tag %s", ref)), nil
		}
		if !pending {
			sctx.Log(fmt.Sprintf("all %d checks passed for tag %s", len(checks), ref))
			return &pipeline.StepOutcome{}, nil
		}
		if !unlimited && now().Sub(started) >= timeout {
			return ciFailureOutcome(nil, false, fmt.Sprintf("CI for tag %s did not settle before the timeout (%d checks reported)", ref, len(checks))), nil
		}
		interval := s.pollIntervalOverride
		if interval == 0 {
			interval = pollInterval(now().Sub(started))
		}
		wait := s.waitForNextPoll
		if wait == nil {
			wait = func(ctx context.Context, d time.Duration) error {
				select {
				case <-time.After(d):
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		if err := wait(sctx.Ctx, interval); err != nil {
			return nil, err
		}
	}
}
