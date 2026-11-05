package steps

import (
	"context"
	"errors"
	"os"
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

func TestPROwnershipUpdateSettlesAfterLatestAuthorEdit(t *testing.T) {
	t.Parallel()
	content, appendix := ownedFixture(t)
	latest := strings.Replace(content.Body, "Human checked this", "Human updated checkbox 🏷️", 1) + "\nAuthor footer"
	host := &settlingOwnershipHost{ownershipRaceHost: &ownershipRaceHost{body: content.Body}}
	host.read = func(h *ownershipRaceHost) error {
		if h.reads == 1 {
			h.body = latest
		}
		return nil
	}
	host.readback = func(h *settlingOwnershipHost) (string, error) {
		if h.postReads == 1 {
			return latest, nil
		}
		return h.body, nil
	}
	err := updateOwnedPR(&pipeline.StepContext{Ctx: context.Background()}, host, &scm.PR{Number: "42"}, scm.PRContent{Body: content.Body}, "", "", appendix+"\nNew evidence", 0)
	if err != nil || host.writes != 1 || host.reads != 2 || host.postReads != 2 {
		t.Fatalf("latest pre-write body was not allowed to settle: err=%v, writes=%d, pre/post reads=%d/%d", err, host.writes, host.reads, host.postReads)
	}
	if !strings.Contains(host.body, "Human updated checkbox 🏷️") || !strings.HasSuffix(host.body, "\nAuthor footer") {
		t.Fatal("latest author edits were lost")
	}
}

func TestPROwnershipUpdateSettleRefusesUnverifiedBodies(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"stale", "foreign-author", "foreign-appendix", "ownership-marker", "media-url", "foreign-after-stale", "read-error", "read-error-after-stale", "write-error"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			content, appendix := ownedFixture(t)
			initial := content.Body + "\n\nAuthor footer 🏷️\n"
			next := appendix + "\n![Выбор бина](https://github.com/user-attachments/assets/22222222-2222-4222-8222-222222222222)"
			evidence := []byte("Recorded media evidence retained for recovery.\n" + next)
			evidenceDir := t.TempDir()
			evidenceFile := writeEvidenceFile(t, evidenceDir, "recorded-evidence.md", evidence)
			host := &settlingOwnershipHost{ownershipRaceHost: &ownershipRaceHost{body: initial}}
			wantReads := 1
			switch mode {
			case "stale":
				wantReads = 3
			case "foreign-after-stale", "read-error-after-stale":
				wantReads = 2
			case "write-error":
				wantReads = 0
				host.writeError = errors.New("uncertain write")
			}
			preservedBody := ""
			submittedBody := ""
			host.readback = func(h *settlingOwnershipHost) (string, error) {
				if h.postReads == 1 {
					submittedBody = h.body
				}
				// An indiscriminate retry would see the submitted body next and
				// incorrectly turn a foreign edit or failed read into success.
				if h.postReads > wantReads {
					return submittedBody, nil
				}
				if mode == "stale" || (strings.HasSuffix(mode, "after-stale") && h.postReads == 1) {
					return initial, nil
				}
				switch mode {
				case "read-error", "read-error-after-stale":
					return "", errors.New("readback unavailable")
				case "foreign-author", "foreign-after-stale":
					h.body += "\nForeign author note 📌"
				case "foreign-appendix":
					h.body = strings.Replace(h.body, "Low recorded risk.", "Foreign evidence edit", 1)
				case "ownership-marker":
					h.body = strings.Replace(h.body, prAppendixEnd, "", 1)
				case "media-url":
					h.body = strings.Replace(h.body, "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", 1)
				}
				preservedBody = h.body
				return h.body, nil
			}
			err := updateOwnedPR(&pipeline.StepContext{Ctx: context.Background(), EvidenceDir: evidenceDir}, host, &scm.PR{Number: "42"}, scm.PRContent{Body: initial}, "", "", next, 0)
			if err == nil || host.writes != 1 || host.postReads != wantReads {
				t.Fatalf("unverified publication: err=%v, writes=%d, reads=%d want %d", err, host.writes, host.postReads, wantReads)
			}
			if mode == "write-error" && host.body != initial {
				t.Fatal("failed write changed the prior body")
			}
			if preservedBody != "" && host.body != preservedBody {
				t.Fatal("foreign edit was rewritten after readback")
			}
			if mode == "stale" && !strings.Contains(host.body, next) {
				t.Fatal("uncertain publication was rolled back")
			}
			if got, err := os.ReadFile(evidenceFile); err != nil || string(got) != string(evidence) {
				t.Fatalf("recovery evidence changed: %q, %v", got, err)
			}
		})
	}
}

func TestPROwnershipUpdateSettleHonorsCancellation(t *testing.T) {
	t.Parallel()
	content, appendix := ownedFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host := &settlingOwnershipHost{ownershipRaceHost: &ownershipRaceHost{body: content.Body}}
	host.readback = func(*settlingOwnershipHost) (string, error) {
		cancel()
		return content.Body, nil
	}
	err := updateOwnedPR(&pipeline.StepContext{Ctx: ctx}, host, &scm.PR{Number: "42"}, scm.PRContent{Body: content.Body}, "", "", appendix+"\nNew evidence", 0)
	if !errors.Is(err, context.Canceled) || host.writes != 1 || host.postReads != 1 {
		t.Fatalf("canceled verification kept running: err=%v, writes=%d, reads=%d", err, host.writes, host.postReads)
	}
}
