package steps

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRTemplateProviderCompositionUpdateAndRestamp(t *testing.T) {
	t.Parallel()
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderGitLab, scm.ProviderGitea, scm.ProviderForgejo, scm.ProviderAzureDevOps, scm.ProviderBitbucket} {
		t.Run(string(provider), func(t *testing.T) {
			sctx, ag, _ := templateTestContext(t)
			step := &PRStep{}
			budget := scm.MaxPRBodyChars(provider)
			created, err := step.buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, provider, budget)
			if err != nil {
				t.Fatal(err)
			}
			original, err := parsePROwnedBody(created.Body)
			if err != nil || !original.managed || !strings.Contains(original.before, "# Overview") {
				t.Fatalf("owned creation: %+v %v", original, err)
			}
			if strings.Count(created.Body, pipelineAttestationCommentPrefix) != 1 {
				t.Fatal("missing/duplicate declaration")
			}
			if got := parsePipelineAttestationForTest(t, created.Body).HeadSHA; got != sctx.Run.HeadSHA {
				t.Fatal("creation bound to wrong head")
			}
			if provider == scm.ProviderBitbucket && (!strings.Contains(original.appendix, "```text\n"+pipelineAttestationCommentPrefix) || strings.Contains(original.appendix, "<details>")) {
				t.Fatal("Bitbucket evidence lost Markdown skin or visible exact declaration")
			}
			// Human edits survive a later template removal, with no new agent draft.
			sctx.Config.PR.Template = ""
			author := "# Human heading\n\n- [x] Human signed off\n😀 café\nCloses https://example.test/issues/7\n\n"
			host := &attestationTestHost{provider: provider, title: "Draft: Author's title", body: author + wrapPRAppendix(original.appendix) + "\nAfter appendix: human note"}
			old := host.body
			appendix, err := step.buildPRAppendix(sctx, provider)
			if err != nil {
				t.Fatal(err)
			}
			if err := updateOwnedPR(sctx, host, &scm.PR{Number: "42"}, scm.PRContent{Title: host.title, Body: old}, "", "", appendix+"\nNew recorded fact.", budget); err != nil {
				t.Fatal(err)
			}
			parts, err := parsePROwnedBody(host.body)
			if err != nil || parts.before != author || parts.after != "\nAfter appendix: human note" || host.title != "Draft: Author's title" || len(ag.calls) != 1 {
				t.Fatalf("author ownership changed: %+v %v", parts, err)
			}
			newHead := strings.Repeat("ab", 20)
			if err := restampPRAttestation(context.Background(), host, &scm.PR{Number: "42"}, newHead, nil); err != nil {
				t.Fatal(err)
			}
			if got := parsePipelineAttestationForTest(t, host.body).HeadSHA; got != newHead {
				t.Fatal("restamp stale")
			}
			rebound, err := parsePROwnedBody(host.body)
			if err != nil || rebound.before != parts.before || rebound.after != parts.after || host.updated.Title != "" {
				t.Fatalf("restamp lost author/ownership: %+v %v", rebound, err)
			}
		})
	}
}

func TestPRTemplateAzureRestampRefusesOverflowBeforeWrite(t *testing.T) {
	t.Parallel()
	content, appendix := ownedFixture(t)
	// Fill the body almost to the UTF-16 cap, including non-BMP text.
	padding := strings.Repeat("😀", (3995-scm.PRBodyLen(content.Body))/2)
	parts, err := parsePROwnedBody(content.Body)
	if err != nil {
		t.Fatal(err)
	}
	parts.before += padding + "\n"
	content, err = composeOwnedPRContent(parts, "", appendix, 4000)
	if err != nil {
		t.Fatal(err)
	}
	host := &attestationTestHost{provider: scm.ProviderAzureDevOps, body: content.Body}
	steps := []*db.StepResult{}
	for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR} {
		steps = append(steps, &db.StepResult{StepName: name, Status: types.StepStatusCompleted})
	}
	if err := restampPRAttestationWithSteps(context.Background(), host, &scm.PR{Number: "42"}, strings.Repeat("ab", 20), steps, nil); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("overflow not refused: %v", err)
	}
	if host.updates != 0 || host.body != content.Body {
		t.Fatal("overflow reached clamping adapter")
	}
}
