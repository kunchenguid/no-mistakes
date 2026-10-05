package steps

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
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

// monitorTagChecks waits for the checks the tag push triggered on its commit.
// A tag is delivered only when at least one check reported and all of them
// settled without failing; failures, providers without tag checks, and
// timeouts with nothing settled park for the user instead of passing.
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
