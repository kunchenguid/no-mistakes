package bitbucket

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPRRawContentLifecyclePreservesTitleDraftAndMarkdown(t *testing.T) {
	t.Parallel()
	title, body, draft := "Human title", "", false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			var data map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
				t.Error(err)
			}
			if r.Method == http.MethodPut {
				if _, ok := data["title"]; ok {
					t.Error("body-only PUT sent title")
				}
				if _, ok := data["draft"]; ok {
					t.Error("body-only PUT sent draft")
				}
			}
			if v, ok := data["title"]; ok {
				_ = json.Unmarshal(v, &title)
			}
			if v, ok := data["draft"]; ok {
				_ = json.Unmarshal(v, &draft)
			}
			_ = json.Unmarshal(data["description"], &body)
		}
		// Official Cloud raw-read shape, intentionally unlike the write payload.
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "title": title, "draft": draft, "summary": map[string]any{"raw": body, "html": "not the raw author text"}, "links": map[string]any{"html": map[string]string{"href": "https://bitbucket.org/owner/repo/pull-requests/7"}}})
	}))
	defer server.Close()
	h := NewHost(&Client{baseURL: server.URL, httpClient: server.Client()}, RepoRef{Workspace: "owner", RepoSlug: "repo"}, true)
	pr, err := h.CreatePR(context.Background(), "feature", "main", scm.PRContent{Title: title, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"", "# Human\n\n😀 café\nCloses owner/repo#9\n"} {
		if _, err := h.UpdatePR(context.Background(), pr, scm.PRContent{Body: want}); err != nil {
			t.Fatal(err)
		}
		got, err := h.GetPRContent(context.Background(), pr)
		if err != nil || got.Body != want || got.Title != title || !draft {
			t.Fatalf("content=%+v draft=%v err=%v", got, draft, err)
		}
	}
}

func TestPRRawContentRejectsUnprovenResponses(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "null", "{}", `{"id":7,"title":"T"}`, `{"id":7,"title":"T","summary":{"raw":null}}`, `{"id":7,"title":"T","summary":{"raw":42}}`, `{"id":8,"title":"T","summary":{"raw":""}}`, `{"id":7,"title":"T","description":"not a raw-read contract"}`, "transport-error"} {
		t.Run(raw, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if raw == "transport-error" {
					w.WriteHeader(503)
				}
				fmt.Fprint(w, raw)
			}))
			defer server.Close()
			h := NewHost(&Client{baseURL: server.URL, httpClient: server.Client()}, RepoRef{Workspace: "owner", RepoSlug: "repo"}, false)
			if _, err := h.GetPRContent(context.Background(), &scm.PR{Number: "7"}); err == nil {
				t.Errorf("accepted %q", raw)
			}
		})
	}
}
