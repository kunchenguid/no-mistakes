package bitbucket

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseRepoRefRejectsLookalikeHosts(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{
			name:    "rejects lookalike host",
			raw:     "https://bitbucket.org.evil.example/workspace/repo.git",
			wantErr: `unsupported Bitbucket host "bitbucket.org.evil.example"`,
		},
		{
			name: "accepts exact host",
			raw:  "https://bitbucket.org/workspace/repo.git",
		},
		{
			name: "accepts subdomain host",
			raw:  "https://foo.bitbucket.org/workspace/repo.git",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, err := ParseRepoRef(tt.raw)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected error")
				}
				if err.Error() != tt.wantErr {
					t.Fatalf("error = %q, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRepoRef returned error: %v", err)
			}
			if repo != (RepoRef{Workspace: "workspace", RepoSlug: "repo"}) {
				t.Fatalf("repo = %#v, want workspace/repo", repo)
			}
		})
	}
}

func TestListPRStatusesFollowsPagination(t *testing.T) {
	repo := RepoRef{Workspace: "test", RepoSlug: "repo"}
	var pageCalls int

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/2.0/repositories/test/repo/pullrequests/42/statuses" {
			t.Fatalf("path = %q, want %q", r.URL.Path, "/2.0/repositories/test/repo/pullrequests/42/statuses")
		}
		pageCalls++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "", "1":
			_, _ = w.Write([]byte(`{"values":[{"name":"build","state":"SUCCESSFUL"}],"next":"` + server.URL + `/2.0/repositories/test/repo/pullrequests/42/statuses?page=2"}`))
		case "2":
			_, _ = w.Write([]byte(`{"values":[{"name":"tests","state":"FAILED"}]}`))
		default:
			t.Fatalf("unexpected page query %q", r.URL.RawQuery)
		}
	}))
	defer server.Close()

	client := &Client{
		baseURL: server.URL,
		email:   "test@example.com",
		token:   "token",
		httpClient: &http.Client{
			Timeout: time.Second,
		},
	}

	statuses, err := client.ListPRStatuses(context.Background(), repo, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 {
		t.Fatalf("len(statuses) = %d, want 2", len(statuses))
	}
	if statuses[0].Name != "build" || statuses[1].Name != "tests" {
		t.Fatalf("statuses = %#v, want build then tests", statuses)
	}
	if pageCalls != 2 {
		t.Fatalf("pageCalls = %d, want 2", pageCalls)
	}
}

func TestListPRStatusesRejectsCrossOriginPagination(t *testing.T) {
	repo := RepoRef{Workspace: "test", RepoSlug: "repo"}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"values":[{"name":"build","state":"SUCCESSFUL"}],"next":"https://evil.example/2.0/repositories/test/repo/pullrequests/42/statuses?page=2"}`)
	}))
	defer server.Close()

	client := &Client{
		baseURL: server.URL,
		email:   "test@example.com",
		token:   "token",
		httpClient: &http.Client{
			Timeout: time.Second,
		},
	}

	_, err := client.ListPRStatuses(context.Background(), repo, 42)
	if err == nil {
		t.Fatal("expected cross-origin pagination to fail")
	}
	if !strings.Contains(err.Error(), "cross-origin") {
		t.Fatalf("error = %q, want cross-origin validation failure", err)
	}
}
