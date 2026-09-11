package steps

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func ownedFixture(t *testing.T) (prContent, string) {
	t.Helper()
	appendix := "## Risk Assessment\n\nLow recorded risk.\n\n## Testing\n\nRecorded evidence link.\n\n" + compliantPipelineBody(t, testPipelineHeadSHA)
	content, err := composeOwnedPRContent(prOwnedBody{before: "## Testing\n\n- [x] Human checked this\n\nCloses test/repo#7"}, "feat: story", appendix, 0)
	if err != nil {
		t.Fatal(err)
	}
	return content, appendix
}

func TestPROwnershipNeverInfersFromHeadings(t *testing.T) {
	t.Parallel()
	body := "Mention no-mistakes-pr-appendix by name without claiming ownership.\n\n## Intent\n\nAuthor intent\n\n## Risk Assessment\n\nAuthor risk\n\n## Tests\n\n- [x] Human checkbox\n\n## Pipeline\n\nRelease plan\nCloses test/repo#1"
	parts, err := parsePROwnedBody(body)
	if err != nil || parts.managed || parts.before != body {
		t.Fatalf("heading resemblance claimed author content: %+v, %v", parts, err)
	}
	_, appendix := ownedFixture(t)
	got, err := composeOwnedPRContent(parts, "", appendix, 0)
	if err != nil || !strings.HasPrefix(got.Body, body) {
		t.Fatalf("author headings removed: %s, %v", got.Body, err)
	}
}

func TestPROwnershipRejectsAmbiguityAndEdits(t *testing.T) {
	t.Parallel()
	content, _ := ownedFixture(t)
	cases := map[string]string{
		"edited evidence":     strings.Replace(content.Body, "Low recorded risk.", "Low recorded risk. Human note.", 1),
		"missing end":         strings.Replace(content.Body, prAppendixEnd, "", 1),
		"missing start":       strings.Replace(content.Body, prAppendixStart, "", 1),
		"malformed digest":    strings.Replace(content.Body, "sha256=", "sha256=z", 1),
		"empty payload":       prAppendixStart + strings.Repeat("0", 64) + " -->\n" + prAppendixEnd,
		"truncated":           prAppendixStart,
		"reversed":            prAppendixEnd + "\n" + prAppendixStart,
		"duplicate":           content.Body + "\n" + content.Body,
		"quoted block":        "```markdown\n" + content.Body + "\n```",
		"tilde quote":         "~~~~\n" + content.Body + "\n~~~~",
		"indented marker":     strings.Replace(content.Body, prAppendixStart, "    "+prAppendixStart, 1),
		"spacing change":      strings.Replace(content.Body, prAppendixStart, "<!--  no-mistakes-pr-appendix:v1 sha256=", 1),
		"inline marker":       strings.Replace(content.Body, prAppendixStart, "quote "+prAppendixStart, 1),
		"foreign attestation": content.Body + "\n" + buildPipelineAttestation(nil, nil, testPipelineHeadSHA),
		"unowned legacy":      compliantPipelineBody(t, testPipelineHeadSHA),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePROwnedBody(body); err == nil {
				t.Fatal("ambiguous/editable content was claimed as generated")
			}
		})
	}
}

func TestPROwnershipFailsSizePressureWithoutTruncation(t *testing.T) {
	t.Parallel()
	_, appendix := ownedFixture(t)
	for _, parts := range []prOwnedBody{
		{before: strings.Repeat("author\n", maxPullRequestBodyBytes)},
		{before: "author", after: strings.Repeat("closing refs\n", maxPullRequestBodyBytes), managed: true},
	} {
		if _, err := composeOwnedPRContent(parts, "", appendix, 0); err == nil {
			t.Fatal("oversized author content was truncated")
		}
	}
	if _, err := composeOwnedPRContent(prOwnedBody{before: "author"}, "", appendix+strings.Repeat("evidence\n", maxPullRequestBodyBytes), 0); err == nil {
		t.Fatal("oversized evidence was dropped")
	}
	if _, err := composeOwnedPRContent(prOwnedBody{before: strings.Repeat("😀", 100)}, "", appendix, 400); err == nil {
		t.Fatal("provider character cap ignored")
	}
}

func TestPROwnershipPublicationRedactionPrecedesDigest(t *testing.T) {
	t.Parallel()
	_, appendix := ownedFixture(t)
	appendix += "\n\nEvidence /Users/private/evidence.png\n```text\n" + prAppendixEnd + "\n```"
	got, err := composeOwnedPRContent(prOwnedBody{before: "## Overview\n\n/home/person/work\n\n", after: "\nC:\\Users\\person\\notes", managed: true}, "feat: /Users/person/path", appendix, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"/Users/", "/home/person", `C:\Users\person`} {
		if strings.Contains(got.Title+got.Body, private) {
			t.Fatalf("published home path %q", private)
		}
	}
	if _, err := parsePROwnedBody(got.Body); err != nil {
		t.Fatalf("redaction invalidated integrity guard: %v", err)
	}
	if strings.Count(got.Body, pipelineAttestationCommentPrefix) != 1 || strings.Count(got.Body, prAppendixNamespace) != 2 {
		t.Fatal("quoted evidence markers shadowed actual ownership/attestation")
	}
}

type ownershipRaceHost struct {
	scm.Host
	body       string
	reads      int
	writes     int
	read       func(*ownershipRaceHost) error
	writeError error
	afterWrite string
}

func (h *ownershipRaceHost) GetPRContent(context.Context, *scm.PR) (scm.PRContent, error) {
	h.reads++
	if h.read != nil {
		if err := h.read(h); err != nil {
			return scm.PRContent{}, err
		}
	}
	return scm.PRContent{Title: "Author title", Body: h.body}, nil
}

func (h *ownershipRaceHost) UpdatePR(_ context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	h.writes++
	if content.Title != "" {
		return nil, errors.New("must leave author title alone")
	}
	if h.writeError != nil {
		return nil, h.writeError
	}
	h.body = content.Body + h.afterWrite
	return pr, nil
}

func TestPROwnershipUpdateMergesLatestAuthorEdits(t *testing.T) {
	t.Parallel()
	content, appendix := ownedFixture(t)
	latest := strings.Replace(content.Body, "Human checked this", "Human updated checkbox label", 1) + "\n\nFixes test/other#9"
	host := &ownershipRaceHost{body: content.Body, read: func(h *ownershipRaceHost) error {
		if h.reads == 1 {
			h.body = latest
		}
		return nil
	}}
	sctx := &pipeline.StepContext{Ctx: context.Background()}
	if err := updateOwnedPR(sctx, host, &scm.PR{Number: "42"}, scm.PRContent(content), "", appendix+"\nNew recorded fact.", 0); err != nil {
		t.Fatal(err)
	}
	if host.writes != 1 || !strings.Contains(host.body, "Human updated checkbox label") || !strings.HasSuffix(host.body, "Fixes test/other#9") || !strings.Contains(host.body, "New recorded fact.") {
		t.Fatalf("lost latest body: writes=%d, %s", host.writes, host.body)
	}
}

func TestPROwnershipUpdateFailuresNeverReadAsSuccess(t *testing.T) {
	t.Parallel()
	content, appendix := ownedFixture(t)
	for _, mode := range []string{"read-error", "write-error", "verify-error", "verify-divergence", "keeps-changing", "edited-owned", "legacy", "size"} {
		t.Run(mode, func(t *testing.T) {
			host := &ownershipRaceHost{body: content.Body}
			initial := content
			wantWrites := 0
			switch mode {
			case "read-error":
				host.read = func(*ownershipRaceHost) error { return errors.New("read unavailable") }
			case "write-error":
				host.writeError = errors.New("uncertain write")
				wantWrites = 1
			case "verify-error":
				host.read = func(h *ownershipRaceHost) error {
					if h.writes > 0 {
						return errors.New("verify unavailable")
					}
					return nil
				}
				wantWrites = 1
			case "verify-divergence":
				host.afterWrite = "\nConcurrent author note"
				wantWrites = 1
			case "keeps-changing":
				host.read = func(h *ownershipRaceHost) error { h.body += fmt.Sprintf("\nAuthor edit %d", h.reads); return nil }
			case "edited-owned":
				initial.Body = strings.Replace(content.Body, "Low recorded risk.", "Human note inside evidence", 1)
			case "legacy":
				initial.Body = compliantPipelineBody(t, testPipelineHeadSHA)
			case "size":
				initial.Body = strings.Repeat("Author content\n", maxPullRequestBodyBytes)
			}
			err := updateOwnedPR(&pipeline.StepContext{Ctx: context.Background()}, host, &scm.PR{Number: "42"}, scm.PRContent(initial), "", appendix+"\nNew fact", 0)
			if err == nil || host.writes != wantWrites {
				t.Fatalf("err=%v, writes=%d want %d", err, host.writes, wantWrites)
			}
		})
	}
}

func TestPROwnershipRestampPreservesAuthorsAndConsumerContract(t *testing.T) {
	t.Parallel()
	content, _ := ownedFixture(t)
	content.Body += "\n\nFixes test/other#9"
	parts, err := parsePROwnedBody(content.Body)
	if err != nil {
		t.Fatal(err)
	}
	newHead := strings.Repeat("ab", 20)
	host := &attestationTestHost{body: content.Body, title: "Author's title"}
	if err := restampPRAttestation(context.Background(), host, &scm.PR{Number: "42"}, newHead, nil); err != nil {
		t.Fatal(err)
	}
	rebound, err := parsePROwnedBody(host.body)
	if err != nil || rebound.before != parts.before || rebound.after != parts.after || host.title != "Author's title" {
		t.Fatalf("restamp invalidated ownership or author content: %+v, %v", rebound, err)
	}
	if got, out := runVerifyPy(t, content.Body, newHead); got != "failure" {
		t.Fatalf("stale head passed: %s", out)
	}
	if got, out := runVerifyPy(t, host.body, newHead); got != "success" {
		t.Fatalf("restamped body failed consumer: %s", out)
	}
	host.body = strings.Replace(host.body, "Low recorded risk.", "Human note inside evidence", 1)
	host.updates = 0
	if err := restampPRAttestation(context.Background(), host, &scm.PR{Number: "42"}, testPipelineHeadSHA, nil); err == nil || host.updates != 0 {
		t.Fatalf("edited evidence overwritten during restamp: err=%v, writes=%d", err, host.updates)
	}
}
