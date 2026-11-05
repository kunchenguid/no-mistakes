package steps

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Readback is independent of the stored body, like a stale forge read replica.
// The embedded host records writes and refuses title changes.
type settlingOwnershipHost struct {
	*ownershipRaceHost
	postReads int
	readback  func(*settlingOwnershipHost) (string, error)
}

func (h *settlingOwnershipHost) GetPRContent(ctx context.Context, pr *scm.PR) (scm.PRContent, error) {
	if h.writes == 0 {
		return h.ownershipRaceHost.GetPRContent(ctx, pr)
	}
	h.postReads++
	body, err := h.readback(h)
	return scm.PRContent{Title: "Author title", Body: body}, err
}

func TestPROwnershipUpdateSettlesOwnWrite(t *testing.T) {
	t.Parallel()
	for _, label := range []string{"Recorded screenshot", "Выбор бина для печати 🏷️"} {
		for _, staleReads := range []int{0, 1, 2} {
			t.Run(label+strings.Repeat("/stale", staleReads), func(t *testing.T) {
				t.Parallel()
				_, appendix := ownedFixture(t)
				before := "## Author testing\n\n- [x] " + label + "\n\n"
				after := "\n\nAuthor footer: " + label + "\nCloses test/other#9\n"
				oldURL := "https://github.com/user-attachments/assets/11111111-1111-4111-8111-111111111111"
				newURL := "https://github.com/user-attachments/assets/22222222-2222-4222-8222-222222222222"
				initial := before + wrapPRAppendix(appendix+"\n!["+label+"]("+oldURL+")") + after
				next := appendix + "\n![" + label + "](" + newURL + ")"
				host := &settlingOwnershipHost{ownershipRaceHost: &ownershipRaceHost{body: initial}}
				host.readback = func(h *settlingOwnershipHost) (string, error) {
					if h.postReads <= staleReads {
						return initial, nil
					}
					return h.body, nil
				}
				err := updateOwnedPR(&pipeline.StepContext{Ctx: context.Background()}, host, &scm.PR{Number: "42"}, scm.PRContent{Body: initial}, "", "", next, 0)
				if err != nil {
					t.Fatalf("own write did not settle: %v (writes=%d, post-write reads=%d)", err, host.writes, host.postReads)
				}
				if host.writes != 1 || host.postReads != staleReads+1 {
					t.Fatalf("write replay or missing verification: writes=%d, post-write reads=%d", host.writes, host.postReads)
				}
				parts, err := parsePROwnedBody(host.body)
				if err != nil || !parts.managed || parts.before != before || parts.after != after || parts.appendix != next {
					t.Fatalf("author bytes, attachment URL or appendix digest changed: %+v, %v", parts, err)
				}
				if got, out := runVerifyPy(t, host.body, testPipelineHeadSHA); got != "success" {
					t.Fatalf("settled attestation failed consumer: %s", out)
				}
			})
		}
	}
}
