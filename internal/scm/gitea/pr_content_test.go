package gitea

import (
	"context"
	"encoding/json"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"strings"
	"testing"
)

func TestPRRawContentAndBodyOnlyUpdate(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"", "# Human\n\n😀 café\nCloses owner/repo#7\n"} {
		raw, _ := json.Marshal(map[string]any{"number": 7, "title": "WIP: Human title", "body": body})
		h := New(giteaTestCmdFactory(map[string]giteaTestResponse{
			"tea api --login work /repos/owner/repo/pulls/7":                                           {stdout: string(raw)},
			strings.TrimSpace("tea pulls edit 7 --repo owner/repo --login work --description " + body): {stdout: "updated"},
		}), nil, "gitea.example.com", "work", "owner/repo")
		pr := &scm.PR{Number: "7"}
		got, err := h.GetPRContent(context.Background(), pr)
		if err != nil || got.Body != body || got.Title != "WIP: Human title" {
			t.Fatalf("content=%+v err=%v", got, err)
		}
		if _, err := h.UpdatePR(context.Background(), pr, scm.PRContent{Body: body}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPRRawContentRejectsUnprovenResponses(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "null", "{}", `{"number":7,"title":"T"}`, `{"number":7,"title":"T","body":null}`, `{"number":7,"title":"T","body":42}`, `{"number":8,"title":"T","body":""}`, `{"index":7,"title":"T","body":"formatted view is not raw API"}`} {
		h := New(giteaTestCmdFactory(map[string]giteaTestResponse{"tea api --login work /repos/owner/repo/pulls/7": {stdout: raw}}), nil, "gitea.example.com", "work", "owner/repo")
		if _, err := h.GetPRContent(context.Background(), &scm.PR{Number: "7"}); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	h := New(giteaTestCmdFactory(map[string]giteaTestResponse{"tea api --login work /repos/owner/repo/pulls/7": {stdout: `{"number":7,"title":"T","body":""}`, code: 1}}), nil, "gitea.example.com", "work", "owner/repo")
	if _, err := h.GetPRContent(context.Background(), &scm.PR{Number: "7"}); err == nil {
		t.Fatal("accepted failed transport")
	}
}
