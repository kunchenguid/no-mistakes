package steps

import (
	"strings"
	"testing"
)

func TestPRTemplateBitbucketVisibleAttestationUsesExistingConsumer(t *testing.T) {
	t.Parallel()
	content, appendix := ownedFixture(t)
	start := strings.Index(appendix, pipelineAttestationCommentPrefix)
	end := start + strings.Index(appendix[start:], "-->") + len("-->")
	appendix = appendix[:start] + "```text\n" + appendix[start:end] + "\n```" + appendix[end:]
	parts, err := parsePROwnedBody(content.Body)
	if err != nil {
		t.Fatal(err)
	}
	content, err = composeOwnedPRContent(parts, "", appendix, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, out := runVerifyPy(t, content.Body, testPipelineHeadSHA); got != "success" {
		t.Fatalf("visible exact declaration failed existing consumer: %s", out)
	}
}

// Exercise the twg-CLI adapter + PR step routing, including ownership discovery
// after configuration removal and visible (not HTML-only) machine evidence.
func TestPRTemplateBitbucketCreateReadbackUpdateAndPrePush(t *testing.T) {
	t.Parallel()
	sctx, ag, _ := templateTestContext(t)
	api := newFakeBitbucketAPI(t, 0, "")

	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Env = api.Env()
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	body := api.lastDescription(t)
	if api.logCount("pull-requests create") != 1 || !strings.Contains(body, "```text\n"+pipelineAttestationCommentPrefix) {
		t.Fatal("no owned text declaration created")
	}
	api.setTitle(t, "Human changed title")
	parts, err := parsePROwnedBody(body)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "# Human edited template\n\n- [x] Actual human signoff\n😀\n\n"
	suffix := "\nCloses https://example.test/issues/9"
	body = prefix + wrapPRAppendix(parts.appendix) + suffix
	// The PR step's own re-read of body happens through the fake twg's state
	// file, not this local variable; seed the state's description so the
	// human edit is what the next Execute reads back.
	api.setDescription(t, body)
	sctx.Config.PR.Template = ""
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	newHead := strings.Repeat("bc", 20)
	if err := attestHeadBeforePush(sctx, newHead, nil); err != nil {
		t.Fatal(err)
	}
	body = api.lastDescription(t)
	rebound, err := parsePROwnedBody(body)
	title := api.title(t)
	if err != nil || rebound.before != prefix || rebound.after != suffix || title != "Human changed title" || api.puts(t) != 1 || len(ag.calls) != 1 {
		t.Fatalf("lifecycle lost ownership: %+v %v title=%q puts=%d calls=%d", rebound, err, title, api.puts(t), len(ag.calls))
	}
	if got := parsePipelineAttestationForTest(t, body).HeadSHA; got != newHead {
		t.Fatal("pre-push left old head")
	}
}
