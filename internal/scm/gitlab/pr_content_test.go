package gitlab

import (
	"context"
	"encoding/json"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"strings"
	"testing"
)

func TestPRRawContentAndBodyOnlyUpdate(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"", "# Human\n\n😀 café\nCloses group/repo#7\n"} {
		raw, _ := json.Marshal(map[string]any{"iid": 7, "title": "Draft: Human title", "description": body})
		h := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{
			"glab mr view 7 --output json":                              {stdout: string(raw)},
			strings.TrimSpace("glab mr update 7 --description " + body): {stdout: "updated"},
		}), nil, "", "")
		pr := &scm.PR{Number: "7"}
		got, err := h.GetPRContent(context.Background(), pr)
		if err != nil || got.Body != body || got.Title != "Draft: Human title" {
			t.Fatalf("content=%+v err=%v", got, err)
		}
		if _, err := h.UpdatePR(context.Background(), pr, scm.PRContent{Body: body}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPRRawContentRejectsUnprovenResponses(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "null", "{}", `{"iid":7,"title":"T"}`, `{"iid":7,"title":"T","description":null}`, `{"iid":7,"title":"T","description":42}`, `{"iid":8,"title":"T","description":""}`} {
		h := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{"glab mr view 7 --output json": {stdout: raw}}), nil, "", "")
		if _, err := h.GetPRContent(context.Background(), &scm.PR{Number: "7"}); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	h := New(gitlabTestCmdFactory(map[string]gitlabTestResponse{"glab mr view 7 --output json": {stdout: `{"iid":7,"title":"T","description":""}`, code: 1}}), nil, "", "")
	if _, err := h.GetPRContent(context.Background(), &scm.PR{Number: "7"}); err == nil {
		t.Fatal("accepted failed transport")
	}
}
