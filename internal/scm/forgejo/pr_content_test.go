package forgejo

import (
	"context"
	"encoding/json"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"reflect"
	"strings"
	"testing"
)

func TestPRRawContentAndBodyOnlyUpdate(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"", strings.Repeat("😀 café\n", 120)} {
		raw, _ := json.Marshal(map[string]any{"status": 200, "data": map[string]any{"number": 42, "title": "[WIP] Human title", "body": body}})
		r := &fakeRecorder{responses: []fakeResponse{{stdout: string(raw)}, {stdout: `{"updated":true,"pull_request":` + pullJSON("open", false, testHeadSHA) + `}`}}}
		h := newTestHost(r)
		pr := &scm.PR{Number: "42", URL: testPRURL}
		got, err := h.GetPRContent(context.Background(), pr)
		if err != nil || got.Body != body || got.Title != "[WIP] Human title" {
			t.Fatalf("content=%+v err=%v", got, err)
		}
		want := []string{"api", "GET", "repos/" + testRepo + "/pulls/42", "--base-url", testBaseURL, "--token-env", "FORGEJO_TEST_TOKEN", "--json"}
		if !reflect.DeepEqual(r.calls[0].args, want) {
			t.Fatalf("raw API contract: %q", r.calls[0].args)
		}
		if _, err := h.UpdatePR(context.Background(), pr, scm.PRContent{Body: body}); err != nil {
			t.Fatal(err)
		}
		for _, arg := range r.calls[1].args {
			if arg == "--title" || arg == "--draft" {
				t.Fatalf("body-only update sent %s", arg)
			}
		}
	}
}

func TestPRRawContentRejectsUnprovenResponses(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "null", "{}", `{"status":200,"data":{"number":42,"title":"T"}}`, `{"status":200,"data":{"number":42,"title":"T","body":null}}`, `{"status":200,"data":{"number":42,"title":"T","body":42}}`, `{"status":200,"data":{"number":8,"title":"T","body":""}}`, `{"status":500,"data":{"number":42,"title":"T","body":""}}`} {
		h := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: raw}}})
		if _, err := h.GetPRContent(context.Background(), &scm.PR{Number: "42", URL: testPRURL}); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	h := newTestHost(&fakeRecorder{responses: []fakeResponse{{code: 1}}})
	if _, err := h.GetPRContent(context.Background(), &scm.PR{Number: "42", URL: testPRURL}); err == nil {
		t.Fatal("accepted failed transport")
	}
}

func TestFindPRWithEmptyBaseMatchesAnyBase(t *testing.T) {
	t.Parallel()
	r := &fakeRecorder{responses: []fakeResponse{{stdout: `{"found":true,"pull_request":` + pullJSON("open", false, testHeadSHA) + `,"search_info":{"complete":true,"pages":1,"fetched":1,"total":1}}`}}}
	pr, err := newTestHost(r).FindPR(context.Background(), "feature/forgejo", "")
	if err != nil || pr == nil {
		t.Fatalf("branch-only lookup: %+v %v", pr, err)
	}
	for _, arg := range r.calls[0].args {
		if arg == "--base" {
			t.Fatal("sent an empty base filter")
		}
	}
}
