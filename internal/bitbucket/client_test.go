package bitbucket

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		if got := r.URL.Query().Get("sort"); got != "-created_on" {
			t.Fatalf("sort = %q, want -created_on", got)
		}
		pageCalls++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "", "1":
			_, _ = w.Write([]byte(`{"values":[{"name":"build","state":"SUCCESSFUL"}],"next":"` + server.URL + `/2.0/repositories/test/repo/pullrequests/42/statuses?sort=-created_on&page=2"}`))
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

func TestListPipelinesByCommitFollowsPagination(t *testing.T) {
	repo := RepoRef{Workspace: "test", RepoSlug: "repo"}
	var pageCalls int

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/2.0/repositories/test/repo/pipelines" {
			t.Fatalf("path = %q, want %q", r.URL.Path, "/2.0/repositories/test/repo/pipelines")
		}
		if got := r.URL.Query().Get("target.commit.hash"); got != "abc123" {
			t.Fatalf("target.commit.hash = %q, want abc123", got)
		}
		if got := r.URL.Query().Get("sort"); got != "-created_on" {
			t.Fatalf("sort = %q, want -created_on", got)
		}
		pageCalls++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "", "1":
			_, _ = w.Write([]byte(`{"values":[{"uuid":"{first}"}],"next":"` + server.URL + `/2.0/repositories/test/repo/pipelines?target.commit.hash=abc123&sort=-created_on&page=2"}`))
		case "2":
			_, _ = w.Write([]byte(`{"values":[{"uuid":"{second}"}]}`))
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

	pipelines, err := client.ListPipelinesByCommit(context.Background(), repo, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if len(pipelines) != 2 {
		t.Fatalf("len(pipelines) = %d, want 2", len(pipelines))
	}
	if pipelines[0].UUID != "{first}" || pipelines[1].UUID != "{second}" {
		t.Fatalf("pipelines = %#v, want first then second", pipelines)
	}
	if pageCalls != 2 {
		t.Fatalf("pageCalls = %d, want 2", pageCalls)
	}
}

func TestListPipelineStepsFollowsPagination(t *testing.T) {
	repo := RepoRef{Workspace: "test", RepoSlug: "repo"}
	var pageCalls int

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/2.0/repositories/test/repo/pipelines/{pipe}/steps" {
			t.Fatalf("path = %q, want %q", r.URL.Path, "/2.0/repositories/test/repo/pipelines/{pipe}/steps")
		}
		pageCalls++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "", "1":
			_, _ = w.Write([]byte(`{"values":[{"uuid":"{step-1}"}],"next":"` + server.URL + `/2.0/repositories/test/repo/pipelines/%7Bpipe%7D/steps?page=2"}`))
		case "2":
			_, _ = w.Write([]byte(`{"values":[{"uuid":"{step-2}","state":{"result":{"name":"FAILED"}}}]}`))
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

	steps, err := client.ListPipelineSteps(context.Background(), repo, "{pipe}")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("len(steps) = %d, want 2", len(steps))
	}
	if steps[0].UUID != "{step-1}" || steps[1].UUID != "{step-2}" {
		t.Fatalf("steps = %#v, want step-1 then step-2", steps)
	}
	if pageCalls != 2 {
		t.Fatalf("pageCalls = %d, want 2", pageCalls)
	}
}

func TestGetStepLogCapsResponseToTail(t *testing.T) {
	repo := RepoRef{Workspace: "test", RepoSlug: "repo"}
	const maxLogBytes = 32 * 1024
	prefix := strings.Repeat("a", 4096)
	tail := strings.Repeat("z", maxLogBytes)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/2.0/repositories/test/repo/pipelines/{pipe}/steps/{step}/log" {
			t.Fatalf("path = %q, want %q", r.URL.Path, "/2.0/repositories/test/repo/pipelines/{pipe}/steps/{step}/log")
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(prefix)+len(tail)))
		_, _ = w.Write([]byte(prefix + tail))
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

	logOutput, err := client.GetStepLog(context.Background(), repo, "{pipe}", "{step}")
	if err != nil {
		t.Fatal(err)
	}
	if len(logOutput) != maxLogBytes {
		t.Fatalf("len(logOutput) = %d, want %d", len(logOutput), maxLogBytes)
	}
	if logOutput != tail {
		t.Fatalf("logOutput did not keep the expected tail")
	}
}
