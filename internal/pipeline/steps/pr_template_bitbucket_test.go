package steps

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// Exercise real HTTP adapter + PR step routing, including ownership discovery
// after configuration removal and visible (not HTML-only) machine evidence.
func TestPRTemplateBitbucketCreateReadbackUpdateAndPrePush(t *testing.T) {
	t.Parallel()
	sctx, ag, _ := templateTestContext(t)
	body, title := "", ""
	exists := false
	puts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		collection := "/2.0/repositories/test/repo/pullrequests"
		switch {
		case r.Method == "GET" && r.URL.Path == collection:
			if exists {
				fmt.Fprint(w, `{"values":[{"id":42}]}`)
			} else {
				fmt.Fprint(w, `{"values":[]}`)
			}
			return
		case (r.Method == "POST" && r.URL.Path == collection) || (r.Method == "PUT" && r.URL.Path == collection+"/42"):
			var payload map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if r.Method == "POST" {
				exists = true
				_ = json.Unmarshal(payload["title"], &title)
			} else {
				puts++
				if _, ok := payload["title"]; ok {
					t.Error("author title would be overwritten")
				}
			}
			_ = json.Unmarshal(payload["description"], &body)
		case r.Method == "GET" && r.URL.Path == collection+"/42":
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "title": title, "summary": map[string]any{"raw": body}, "links": map[string]any{"html": map[string]string{"href": "https://bitbucket.org/test/repo/pull-requests/42"}}})
	}))
	defer server.Close()
	sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	sctx.Env = fakeBitbucketEnv(server.URL)
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if !exists || !strings.Contains(body, "```text\n"+pipelineAttestationCommentPrefix) {
		t.Fatal("no owned text declaration created")
	}
	title = "Human changed title"
	parts, err := parsePROwnedBody(body)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "# Human edited template\n\n- [x] Actual human signoff\n😀\n\n"
	suffix := "\nCloses https://example.test/issues/9"
	body = prefix + wrapPRAppendix(parts.appendix) + suffix
	sctx.Config.PR.Template = ""
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	newHead := strings.Repeat("bc", 20)
	if err := attestHeadBeforePush(sctx, newHead, nil); err != nil {
		t.Fatal(err)
	}
	rebound, err := parsePROwnedBody(body)
	if err != nil || rebound.before != prefix || rebound.after != suffix || title != "Human changed title" || puts != 1 || len(ag.calls) != 1 {
		t.Fatalf("lifecycle lost ownership: %+v %v puts=%d calls=%d", rebound, err, puts, len(ag.calls))
	}
	if got := parsePipelineAttestationForTest(t, body).HeadSHA; got != newHead {
		t.Fatal("pre-push left old head")
	}
}
