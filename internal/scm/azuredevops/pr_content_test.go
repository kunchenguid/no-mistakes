package azuredevops

import (
	"context"
	"encoding/json"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"strings"
	"testing"
)

func TestPRRawContentAndBodyOnlyUpdate(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"", "# Human\n\n😀 café\nCloses AB#7\n"} {
		raw, _ := json.Marshal(map[string]any{"pullRequestId": 7, "title": "Human title", "description": body, "isDraft": true})
		var calls []capturedCmd
		h := newCapturingHost(&calls, azdoTestResponse{stdout: string(raw)})
		pr := &scm.PR{Number: "7"}
		got, err := h.GetPRContent(context.Background(), pr)
		if err != nil || got.Body != body || got.Title != "Human title" {
			t.Fatalf("content=%+v err=%v", got, err)
		}
		if len(calls) != 1 || !strings.Contains(strings.Join(calls[0].args, " "), "repos pr show --id 7") {
			t.Fatalf("wrong read: %+v", calls)
		}
		calls = nil
		if _, err := h.UpdatePR(context.Background(), pr, scm.PRContent{Body: body}); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			assertDescriptionRoundTrips(t, calls, body)
		} else if len(calls) != 1 || calls[0].descContent != "" || !calls[0].descExists {
			t.Fatalf("empty description did not use the file transport: %+v", calls)
		}
		args := strings.Join(calls[0].args, " ")
		if strings.Contains(args, "--title") || strings.Contains(args, "--draft") {
			t.Fatalf("body-only update changed title/draft: %s", args)
		}
	}
}

func TestPRRawContentRejectsUnprovenResponses(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "null", "{}", `{"pullRequestId":7,"title":"T"}`, `{"pullRequestId":7,"title":"T","description":null}`, `{"pullRequestId":7,"title":"T","description":42}`, `{"pullRequestId":8,"title":"T","description":""}`} {
		var calls []capturedCmd
		h := newCapturingHost(&calls, azdoTestResponse{stdout: raw})
		if _, err := h.GetPRContent(context.Background(), &scm.PR{Number: "7"}); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	var calls []capturedCmd
	h := newCapturingHost(&calls, azdoTestResponse{code: 1})
	if _, err := h.GetPRContent(context.Background(), &scm.PR{Number: "7"}); err == nil {
		t.Fatal("accepted failed transport")
	}
}
