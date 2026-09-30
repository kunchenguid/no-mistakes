package steps

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type mutationComparisonHost struct {
	scm.Host
	facts  scm.PRFacts
	body   string
	writes int
	reads  int
	onRead func(*mutationComparisonHost)
	err    error
}

func (h *mutationComparisonHost) Provider() scm.Provider { return scm.ProviderGitHub }

func (h *mutationComparisonHost) ReadPRFacts(context.Context, *scm.PR) (scm.PRFacts, error) {
	return h.facts, h.err
}

func (h *mutationComparisonHost) FindOpenPRFacts(context.Context, string, string) ([]scm.PRFacts, error) {
	return nil, nil
}

func (h *mutationComparisonHost) GetPRContent(context.Context, *scm.PR) (scm.PRContent, error) {
	h.reads++
	if h.onRead != nil {
		h.onRead(h)
	}
	return scm.PRContent{Body: h.body}, nil
}

func (h *mutationComparisonHost) UpdatePR(_ context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	h.writes++
	h.body = content.Body
	return pr, nil
}

func mutationComparisonFixture(t *testing.T) (*pipeline.StepContext, *mutationComparisonHost, *scm.PR) {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	url := "https://github.com/test/repo/pull/42"
	selection := pipeline.PRTargetSelection{PRURL: url, SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: head, TargetBranch: "main"}
	if _, err := guardPRContextWithSelection(sctx, types.StepPR, selection, false); err != nil {
		t.Fatal(err)
	}
	sctx.Run.PRURL = &url
	pr := prFromOwnedURL(url)
	host := &mutationComparisonHost{facts: scm.PRFacts{PR: *pr, State: scm.PRStateOpen, SourceRepository: "test/repo", SourceBranch: "feature", HeadSHA: head, BaseBranch: "main"}}
	return sctx, host, pr
}

func TestPRMutationComparisonRejectsChangedLiveEvidence(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(*mutationComparisonHost)
	}{
		{"retarget", func(h *mutationComparisonHost) { h.facts.BaseBranch = "develop" }},
		{"source head", func(h *mutationComparisonHost) { h.facts.HeadSHA = strings.Repeat("a", 40) }},
		{"source repository", func(h *mutationComparisonHost) { h.facts.SourceRepository = "other/repo" }},
		{"closed", func(h *mutationComparisonHost) { h.facts.State = scm.PRStateClosed }},
		{"unavailable", func(h *mutationComparisonHost) { h.err = errors.New("unavailable") }},
	} {
		t.Run(change.name, func(t *testing.T) {
			sctx, host, pr := mutationComparisonFixture(t)
			content, appendix := ownedFixture(t)
			host.body = content.Body
			host.onRead = func(h *mutationComparisonHost) { change.mutate(h) }
			err := updateOwnedPR(sctx, host, pr, scm.PRContent(content), "", "", appendix+"\nNew fact", 0, func() error {
				return verifyPRMutationComparison(sctx, host, pr, sctx.Run.HeadSHA)
			})
			if err == nil || host.writes != 0 {
				t.Fatalf("stale PR update: err=%v writes=%d", err, host.writes)
			}
		})
	}
}

func TestPRMutationComparisonAcceptsCurrentOwnedAndProposedAttestation(t *testing.T) {
	sctx, host, pr := mutationComparisonFixture(t)
	content, appendix := ownedFixture(t)
	host.body = content.Body
	if err := updateOwnedPR(sctx, host, pr, scm.PRContent(content), "", "", appendix+"\nNew fact", 0, func() error {
		return verifyPRMutationComparison(sctx, host, pr, sctx.Run.HeadSHA)
	}); err != nil || host.writes != 1 {
		t.Fatalf("current owned update: %v, writes=%d", err, host.writes)
	}
	gitCmd(t, sctx.WorkDir, "commit", "--allow-empty", "-m", "repair")
	proposed := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
	sctx.Run.HeadSHA = proposed
	host.body = compliantPipelineBody(t, host.facts.HeadSHA)
	if err := restampPRAttestationWithSteps(sctx.Ctx, host, pr, proposed, nil, nil, pipelineAttestationPolicy{}, func() error {
		return verifyPRMutationComparison(sctx, host, pr, proposed)
	}); err != nil || host.writes != 2 {
		t.Fatalf("proposed head restamp: %v, writes=%d", err, host.writes)
	}
}

func TestPRMutationComparisonRejectsTargetAdvanceDuringDraft(t *testing.T) {
	sctx, host, pr := mutationComparisonFixture(t)
	content, appendix := ownedFixture(t)
	host.body = content.Body
	host.onRead = func(h *mutationComparisonHost) {
		if h.reads != 1 {
			return
		}
		gitCmd(t, sctx.WorkDir, "checkout", "main")
		gitCmd(t, sctx.WorkDir, "commit", "--allow-empty", "-m", "advance target")
		gitCmd(t, sctx.WorkDir, "checkout", "feature")
	}
	err := updateOwnedPR(sctx, host, pr, scm.PRContent(content), "", "", appendix+"\nNew fact", 0, func() error {
		return verifyPRMutationComparison(sctx, host, pr, sctx.Run.HeadSHA)
	})
	if err == nil || host.writes != 0 {
		t.Fatalf("target advance was published: err=%v writes=%d", err, host.writes)
	}
}
