package steps

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// mergedHeadTestHost is a provider without merged proof: it can only report
// the head it observes for the pull request, which is exactly what the CI
// step's merged completion has to fall back to on GitLab, Gitea, Azure DevOps
// and the rest.
type mergedHeadTestHost struct {
	scm.Host
	observedHead string
	reads        int
	err          error
}

func (h *mergedHeadTestHost) Provider() scm.Provider { return scm.ProviderGitLab }

func (h *mergedHeadTestHost) Capabilities() scm.Capabilities { return scm.Capabilities{} }

func (h *mergedHeadTestHost) GetChecks(_ context.Context, pr *scm.PR) ([]scm.Check, error) {
	h.reads++
	if h.err != nil {
		return nil, h.err
	}
	pr.ObservedHeadSHA = h.observedHead
	return []scm.Check{{Name: "build", Bucket: scm.CheckBucketPass}}, nil
}

// A merge the provider binds to one of the run's own heads is the run's own
// outcome, even without merged proof.
func TestVerifyMergedHead_ObservedOwnedHeadCompletes(t *testing.T) {
	t.Parallel()
	pr := &scm.PR{Number: "7", URL: "https://gitlab.example/octo/widgets/-/merge_requests/7"}
	host := &mergedHeadTestHost{observedHead: strings.Repeat("a", 40)}

	head, err := verifyMergedHead(context.Background(), host, pr, []string{strings.Repeat("a", 40)})
	if err != nil {
		t.Fatalf("verifyMergedHead() error = %v, want the owned observed head accepted", err)
	}
	if head != strings.Repeat("a", 40) {
		t.Fatalf("merged head = %q, want the observed owned head", head)
	}
	if host.reads != 1 {
		t.Fatalf("head reads = %d, want one", host.reads)
	}
}

// The regression this guards: an external rewrite to B followed by a merge
// before the next poll used to complete the run cleanly on providers without
// merged proof. A merge the provider binds to a head the run owns nowhere is
// refused as a foreign merge instead.
func TestVerifyMergedHead_ObservedForeignHeadIsRefused(t *testing.T) {
	t.Parallel()
	pr := &scm.PR{Number: "7", URL: "https://gitlab.example/octo/widgets/-/merge_requests/7"}
	host := &mergedHeadTestHost{observedHead: strings.Repeat("b", 40)}

	_, err := verifyMergedHead(context.Background(), host, pr, []string{strings.Repeat("a", 40)})
	if !errors.Is(err, scm.ErrHeadChanged) {
		t.Fatalf("verifyMergedHead() error = %v, want the foreign merged head refused", err)
	}
}

// A provider that reports no head at all leaves the merge unproven, and an
// unproven merge is a refusal: a merged completion is a verdict on a head, and
// one nobody can name must never be recorded as a clean pass.
func TestVerifyMergedHead_UnprovenHeadIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		host    *mergedHeadTestHost
		wantErr error
	}{
		{name: "no observation", host: &mergedHeadTestHost{}, wantErr: errMergedHeadUnproven},
		{name: "unreadable read", host: &mergedHeadTestHost{err: context.DeadlineExceeded}, wantErr: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pr := &scm.PR{Number: "7", URL: "https://gitlab.example/octo/widgets/-/merge_requests/7"}
			_, err := verifyMergedHead(context.Background(), tc.host, pr, []string{strings.Repeat("a", 40)})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("verifyMergedHead() error = %v, want %v", err, tc.wantErr)
			}
			if tc.host.err != nil && errors.Is(err, errMergedHeadUnproven) {
				t.Fatalf("verifyMergedHead() error = %v, want the failed read to remain retryable", err)
			}
			if errors.Is(err, scm.ErrHeadChanged) {
				t.Fatalf("verifyMergedHead() error = %v, want not reported as a head change", err)
			}
		})
	}
}

// A run that owns no head cannot identify the merge at all.
func TestVerifyMergedHead_NoOwnHeadsIsRefused(t *testing.T) {
	t.Parallel()
	pr := &scm.PR{Number: "7", URL: "https://gitlab.example/octo/widgets/-/merge_requests/7"}
	host := &mergedHeadTestHost{observedHead: strings.Repeat("a", 40)}

	_, err := verifyMergedHead(context.Background(), host, pr, nil)
	if !errors.Is(err, errMergedHeadUnproven) {
		t.Fatalf("verifyMergedHead() error = %v, want the merge refused without an own head", err)
	}
	if host.reads != 0 {
		t.Fatalf("head reads = %d, want none without a head to look the merge up by", host.reads)
	}
}

// Hosts that advertise merged proof keep answering from it.
func TestVerifyMergedHead_ProvenHostUsesTheProof(t *testing.T) {
	t.Parallel()
	pr := &scm.PR{Number: "42", URL: "https://github.example/octo/widgets/pull/42"}
	head := strings.Repeat("c", 40)
	host := &mergedProofTestHost{proof: scm.MergedProof{Merged: true, Number: "42", URL: pr.URL, HeadSHA: head}}

	got, err := verifyMergedHead(context.Background(), host, pr, []string{head})
	if err != nil {
		t.Fatalf("verifyMergedHead() error = %v", err)
	}
	if got != head {
		t.Fatalf("merged head = %q, want the proven %q", got, head)
	}
}

// headReportingHost is a provider without merged proof that can still name the
// pull request's head, which is the cheaper and more direct read.
type headReportingHost struct {
	scm.Host
	head       string
	err        error
	headReads  int
	checkReads int
}

func (h *headReportingHost) Provider() scm.Provider { return scm.ProviderGitea }

func (h *headReportingHost) Capabilities() scm.Capabilities { return scm.Capabilities{} }

func (h *headReportingHost) GetPRHeadSHA(context.Context, *scm.PR) (string, error) {
	h.headReads++
	if h.err != nil {
		return "", h.err
	}
	return h.head, nil
}

func (h *headReportingHost) GetChecks(_ context.Context, pr *scm.PR) ([]scm.Check, error) {
	h.checkReads++
	pr.ObservedHeadSHA = ""
	return nil, nil
}

// A host that can name the pull request's head answers the merged-head read
// without paying for a check read of a merged pull request.
func TestVerifyMergedHead_UsesTheReportedHead(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("d", 40)
	pr := &scm.PR{Number: "7", URL: "https://gitea.example/octo/widgets/pulls/7"}
	host := &headReportingHost{head: head}

	got, err := verifyMergedHead(context.Background(), host, pr, []string{head})
	if err != nil {
		t.Fatalf("verifyMergedHead() error = %v", err)
	}
	if got != head {
		t.Fatalf("merged head = %q, want the reported %q", got, head)
	}
	if host.headReads != 1 || host.checkReads != 0 {
		t.Fatalf("reads = {head %d, checks %d}, want one head read and no check read", host.headReads, host.checkReads)
	}
}

// The reported head is held to the same ownership rule as the proof: a merge
// at a head the run owns nowhere is refused.
func TestVerifyMergedHead_ReportedForeignHeadIsRefused(t *testing.T) {
	t.Parallel()
	pr := &scm.PR{Number: "7", URL: "https://gitea.example/octo/widgets/pulls/7"}
	host := &headReportingHost{head: strings.Repeat("e", 40)}

	_, err := verifyMergedHead(context.Background(), host, pr, []string{strings.Repeat("d", 40)})
	if !errors.Is(err, scm.ErrHeadChanged) {
		t.Fatalf("verifyMergedHead() error = %v, want the foreign reported head refused", err)
	}
}

// A failed head read falls back to the check observation rather than failing
// outright, and only an unproven merge - neither read naming a head - refuses.
func TestVerifyMergedHead_FallsBackToTheCheckObservation(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("f", 40)
	pr := &scm.PR{Number: "7", URL: "https://gitea.example/octo/widgets/pulls/7"}
	host := &headReportingHost{err: errors.New("pr view unavailable")}
	fallback := &mergedHeadTestHost{observedHead: head}

	got, err := verifyMergedHead(context.Background(), &fallbackFromReaderHost{headReportingHost: host, fallback: fallback}, pr, []string{head})
	if err != nil {
		t.Fatalf("verifyMergedHead() error = %v", err)
	}
	if got != head {
		t.Fatalf("merged head = %q, want the observed %q", got, head)
	}
	if fallback.reads != 1 {
		t.Fatalf("fallback check reads = %d, want one", fallback.reads)
	}
}

// fallbackFromReaderHost reports a head read failure and delegates the check
// observation to the fallback host.
type fallbackFromReaderHost struct {
	*headReportingHost
	fallback *mergedHeadTestHost
}

func (h *fallbackFromReaderHost) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	return h.fallback.GetChecks(ctx, pr)
}

func TestVerifyMergedHead_PrimaryFailureNeedsFallbackEvidence(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name     string
		readErr  error
		fallback string
		wantErr  error
	}{
		{name: "transient primary error", readErr: context.DeadlineExceeded, wantErr: context.DeadlineExceeded},
		{name: "successful response missing head", readErr: scm.ErrPRHeadMissing, wantErr: errMergedHeadUnproven},
		{name: "unsupported primary read", readErr: scm.ErrUnsupported, wantErr: errMergedHeadUnproven},
		{name: "fallback identifies foreign head", readErr: context.DeadlineExceeded, fallback: strings.Repeat("b", 40), wantErr: scm.ErrHeadChanged},
		{name: "fallback identifies owned head", readErr: context.DeadlineExceeded, fallback: head},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host := &fallbackFromReaderHost{
				headReportingHost: &headReportingHost{err: tc.readErr},
				fallback:          &mergedHeadTestHost{observedHead: tc.fallback},
			}
			got, err := verifyMergedHead(context.Background(), host, &scm.PR{Number: "7"}, []string{head})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("merged head = %q, %v; want error %v", got, err, tc.wantErr)
			}
			if tc.wantErr == nil && got != head {
				t.Fatalf("merged head = %q, want %q", got, head)
			}
		})
	}
}
