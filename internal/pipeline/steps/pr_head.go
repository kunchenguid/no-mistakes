package steps

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// verifyPublishedPR is the shared publication postcondition. Read the durable
// successful-push binding, never the executor's potentially stale in-memory run
// or mutable Git HEAD. Refusal leaves that binding and custody evidence intact.
func verifyPublishedPR(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) error {
	run, err := publishedPRPushBinding(sctx, host)
	if err != nil || run == nil {
		return err
	}
	verifier := host.(scm.PRHeadVerifier)
	previous := ""
	publication, ok := sctx.Shared.PublishedPush()
	if ok && publication.RunID == run.ID && publication.RepoID == run.RepoID && publication.Binding == (db.PushBinding{
		HeadSHA:           *run.LastPushedSHA,
		TargetKind:        *run.PushTargetKind,
		TargetFingerprint: *run.PushTargetFingerprint,
		Ref:               *run.PushRef,
	}) {
		previous = publication.PreviousHeadSHA
	}
	var retryUntil time.Time
	for {
		err := verifier.VerifyPRHead(sctx.Ctx, pr, strings.TrimPrefix(*run.PushRef, "refs/heads/"), *run.LastPushedSHA)
		if err == nil {
			return nil
		}
		var changed *scm.PRHeadChangedError
		if previous == "" || strings.EqualFold(previous, *run.LastPushedSHA) || !errors.As(err, &changed) || !strings.EqualFold(changed.HeadSHA, previous) {
			return fmt.Errorf("verify published PR: %w", err)
		}
		if retryUntil.IsZero() {
			retryUntil = time.Now().Add(3 * time.Second)
		}
		remaining := time.Until(retryUntil)
		if remaining <= 0 {
			return fmt.Errorf("verify published PR: head did not catch up within three seconds: %w", err)
		}
		timer := time.NewTimer(min(250*time.Millisecond, remaining))
		select {
		case <-sctx.Ctx.Done():
			timer.Stop()
			return fmt.Errorf("verify published PR: waiting for the pushed head: %w", sctx.Ctx.Err())
		case <-timer.C:
		}
		if !time.Now().Before(retryUntil) {
			return fmt.Errorf("verify published PR: head did not catch up within three seconds: %w", err)
		}
	}
}

func publishedPRPushBinding(sctx *pipeline.StepContext, host scm.Host) (*db.Run, error) {
	if _, ok := host.(scm.PRHeadVerifier); !ok {
		return nil, nil
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		return nil, fmt.Errorf("verify published PR: read successful push binding: %w", err)
	}
	if run == nil || run.RepoID != sctx.Repo.ID || run.LastPushedSHA == nil || strings.TrimSpace(*run.LastPushedSHA) == "" || run.PushActive || run.PushTargetFingerprint == nil || run.PushTargetKind == nil || run.PushRef == nil || run.PushGeneration == nil || *run.PushGeneration < 1 {
		return nil, fmt.Errorf("verify published PR: complete successful push binding is unavailable")
	}
	kind := "upstream"
	if strings.TrimSpace(sctx.Repo.ForkURL) != "" {
		kind = "fork"
	}
	if *run.PushTargetKind != kind || *run.PushTargetFingerprint != branchsync.TargetFingerprint(resolvePushURL(sctx)) || *run.PushRef != normalizedBranchRef(run.Branch) || normalizedBranchRef(run.Branch) != normalizedBranchRef(sctx.Run.Branch) {
		return nil, fmt.Errorf("verify published PR: successful push binding does not match this run's repository target and branch")
	}
	if kind == "fork" {
		// A fork belongs to its parent's forge instance. Equal owner/repository
		// slugs on different hosts cannot identify the pushed repository.
		sourceHost := scm.ResolveHost(sctx.Ctx, sctx.Repo.ForkURL)
		reviewHost := resolvedHost(sctx, sctx.Repo.UpstreamURL)
		if sourceHost == "" || reviewHost == "" || !strings.EqualFold(sourceHost, reviewHost) {
			return nil, fmt.Errorf("verify published PR: successful push binding names a different forge host than the review object")
		}
	}
	return run, nil
}
